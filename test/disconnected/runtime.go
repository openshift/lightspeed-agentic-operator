package disconnected

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func probeLabels(selector metav1.LabelSelector) (map[string]string, error) {
	parsed, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return nil, err
	}
	requirements, _ := parsed.Requirements()
	keys := map[string][]labels.Requirement{}
	for _, r := range requirements {
		keys[r.Key()] = append(keys[r.Key()], r)
	}
	result := map[string]string{}
	for key, rules := range keys {
		candidates := []string{}
		for _, r := range rules {
			candidates = append(candidates, r.Values().List()...)
		}
		// At least one synthetic value lies outside all finite NotIn sets.
		n := len(candidates)
		for i := 0; i <= n; i++ {
			candidates = append(candidates, fmt.Sprintf("probe-%d", i))
		}
		found := false
		options := append([]string{""}, candidates...)
		for index, value := range options {
			candidate := labels.Set{}
			if index != 0 {
				candidate[key] = value
			}
			matches := true
			for _, r := range rules {
				if !r.Matches(candidate) {
					matches = false
					break
				}
			}
			if matches {
				if index != 0 {
					result[key] = value
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("cannot construct policy-selected probe labels")
		}
	}
	return result, nil
}

// Start installs a temporary runtime boundary and registers evidence collection
// before cleanup. Call only after all scenario/base images have been validated.
func (cfg Config) Start(t *testing.T, c client.Client, namespace, image string) {
	t.Helper()
	ctx := context.Background()
	id := os.Getenv("E2E_DISCONNECTED_ID")
	if id == "" {
		t.Fatal("E2E_DISCONNECTED_ID is required for independent cleanup")
	}
	var resources []client.Object
	t.Cleanup(func() {
		if err := cfg.VerifyInference(ctx, c); err != nil {
			t.Errorf("final inference selector check: %v", err)
		}
		cfg.diagnostics(t, c, namespace)
		for i := len(resources) - 1; i >= 0; i-- {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := c.Delete(cleanupCtx, resources[i])
			cancel()
			if client.IgnoreNotFound(err) != nil {
				t.Logf("disconnected cleanup: %v", err)
			}
		}
	})
	create := func(obj client.Object) {
		t.Helper()
		objectLabels := obj.GetLabels()
		if objectLabels == nil {
			objectLabels = map[string]string{}
		}
		objectLabels[OwnedLabel] = id
		obj.SetLabels(objectLabels)
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create disconnected resource %s: %v", obj.GetName(), err)
		}
		resources = append(resources, obj)
	}
	if err := cfg.VerifyInference(ctx, c); err != nil {
		t.Fatal(err)
	}
	dnsIPs, dnsPort, err := serviceAddresses(ctx, c, "openshift-dns", "dns-default", 53)
	if err != nil {
		t.Fatal(err)
	}
	apiIPs, apiPort, err := serviceAddresses(ctx, c, "default", "kubernetes", 443)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := cfg.Policies(namespace, dnsIPs, apiIPs, dnsPort, apiPort)
	if err != nil {
		t.Fatal(err)
	}
	// Policies are additive. Require namespaces without existing egress policies.
	for _, policy := range policies {
		var existing networkingv1.NetworkPolicyList
		if err := c.List(ctx, &existing, client.InNamespace(policy.Namespace)); err != nil {
			t.Fatal(err)
		}
		for _, p := range existing.Items {
			for _, kind := range p.Spec.PolicyTypes {
				if kind == networkingv1.PolicyTypeEgress {
					t.Fatalf("existing egress policy %s/%s invalidates isolated test boundary", p.Namespace, p.Name)
				}
			}
		}
	}
	key, err := os.ReadFile(os.Getenv("E2E_PROVIDER_KEY_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-disconnected-key-", Namespace: namespace}, Data: map[string][]byte{"key": key}}
	create(secret)
	labels, err := probeLabels(cfg.Selector)
	if err != nil {
		t.Fatal(err)
	}
	sas := map[string]*corev1.ServiceAccount{}
	for _, ns := range []string{namespace, cfg.Namespace} {
		if sas[ns] != nil {
			continue
		}
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-disconnected-probe-", Namespace: ns}}
		create(sa)
		sas[ns] = sa
		err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, client.ObjectKeyFromObject(sa), sa); err != nil {
				return false, err
			}
			return len(sa.ImagePullSecrets) > 0, nil
		})
		if err != nil {
			t.Fatalf("probe ServiceAccount registry credentials: %v", err)
		}
	}
	imageNamespace := strings.Split(strings.TrimPrefix(image, cfg.Registry+"/"), "/")[0]
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-disconnected-pull-", Namespace: imageNamespace}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:image-puller"}}
	for _, sa := range sas {
		binding.Subjects = append(binding.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: sa.Name, Namespace: sa.Namespace})
	}
	create(binding)
	probes := []*corev1.Pod{
		probePod(namespace, image, map[string]string{RunLabel: "disconnected-probe"}, secret.Name),
		probePod(cfg.Namespace, image, labels, ""),
	}
	for _, p := range probes {
		sa := sas[p.Namespace]
		p.Spec.ServiceAccountName = sa.Name
		p.Spec.ImagePullSecrets = sa.ImagePullSecrets
		yes := true
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ServiceAccount", Name: sa.Name, UID: sa.UID, Controller: &yes}}
		create(p)
		err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			var pod corev1.Pod
			if err := c.Get(ctx, client.ObjectKeyFromObject(p), &pod); err != nil {
				return false, err
			}
			if pod.Status.Phase == corev1.PodFailed {
				return false, fmt.Errorf("probe failed")
			}
			return pod.Status.Phase == corev1.PodRunning, nil
		})
		if err != nil {
			t.Fatalf("probe startup: %v", err)
		}
		if err := cfg.probe(p, "baseline"); err != nil {
			t.Fatalf("connected canary baseline: %v", err)
		}
	}
	for i := range policies {
		create(&policies[i])
	}
	// Policy propagation is asynchronous. Retry only restricted preflight, never
	// restore egress or retry the product run without its policies.
	for i, p := range probes {
		mode := "restricted"
		if i == 0 {
			mode = "models"
		}
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true, func(context.Context) (bool, error) {
			err := cfg.probe(p, mode)
			if err != nil {
				t.Logf("waiting for restricted preflight: %v", err)
			}
			return err == nil, nil
		})
		if err != nil {
			t.Fatalf("restricted preflight %s: %v", p.Namespace, err)
		}
	}
	t.Log("Disconnected preflight passed: DNS/API/models reachable; baseline HTTPS canary denied from both policies")
}

func probePod(namespace, image string, labels map[string]string, secret string) *corev1.Pod {
	no := false
	yes := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-disconnected-probe-", Namespace: namespace, Labels: labels}, Spec: corev1.PodSpec{AutomountServiceAccountToken: &yes, RestartPolicy: corev1.RestartPolicyNever, SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}, Containers: []corev1.Container{{Name: "probe", Image: image, Command: []string{"python3", "-c", "import time; time.sleep(86400)"}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}, ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"python3", "-c", "raise SystemExit(1)"}}}}}}}}
	// Never Ready: copying inference labels must not add the probe to serving endpoints.
	if secret != "" {
		pod.Spec.Volumes = []corev1.Volume{{Name: "key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}}}}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "key", MountPath: "/var/run/e2e-key", ReadOnly: true}}
	}
	return pod
}

const probeScript = `import http.client, json, os, socket, ssl, sys, urllib.request
from urllib.parse import urlparse
mode, base, model, canary = sys.argv[1:]
u = urlparse(canary)
assert u.scheme == 'https' and u.hostname and not u.username
port = u.port or 443
if mode == 'baseline':
    ips = []
    for ip in sorted(set(x[4][0] for x in socket.getaddrinfo(u.hostname, port, type=socket.SOCK_STREAM))):
        try:
            with socket.create_connection((ip, port), timeout=10) as raw:
                with ssl.create_default_context().wrap_socket(raw, server_hostname=u.hostname) as connection:
                    connection.sendall(('HEAD '+(u.path or '/')+' HTTP/1.1\r\nHost: '+u.hostname+'\r\nConnection: close\r\n\r\n').encode())
                    response = http.client.HTTPResponse(connection)
                    response.begin()
                    assert 200 <= response.status < 400
                    ips.append(ip)
        except OSError:
            continue
    assert ips, 'HTTPS canary is not reachable before policy installation'
    with open('/tmp/e2e-canary.json', 'w') as f: json.dump(ips, f)
else:
    # Prove DNS works, but test denial using the baseline IPs, not DNS errors.
    socket.getaddrinfo('kubernetes.default.svc', 443)
    socket.getaddrinfo(urlparse(base).hostname, urlparse(base).port or 80)
    with open('/tmp/e2e-canary.json') as f: ips = json.load(f)
    for ip in ips:
        try:
            connection = socket.create_connection((ip, port), timeout=4)
        except (TimeoutError, ConnectionRefusedError):
            continue
        else:
            connection.close()
            raise RuntimeError('external TCP egress still permitted')
    token = open('/var/run/secrets/kubernetes.io/serviceaccount/token').read().strip()
    ca = ssl.create_default_context(cafile='/var/run/secrets/kubernetes.io/serviceaccount/ca.crt')
    req = urllib.request.Request('https://kubernetes.default.svc/api', headers={'Authorization': 'Bearer '+token})
    with urllib.request.urlopen(req, context=ca, timeout=15) as response: assert response.status == 200
    if mode == 'models':
        key = open('/var/run/e2e-key/key').read().strip()
        req = urllib.request.Request(base+'/models', headers={'Authorization': 'Bearer '+key})
        with urllib.request.urlopen(req, timeout=30) as response: data = json.load(response)
        assert model in [item['id'] for item in data['data']], 'selected model not advertised'
print('preflight '+mode+' OK')
`

func (cfg Config) probe(pod *corev1.Pod, mode string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	canary := os.Getenv("E2E_EGRESS_CANARY")
	if canary == "" {
		canary = "https://example.com/"
	}
	cmd := exec.CommandContext(ctx, "oc", "exec", "-n", pod.Namespace, pod.Name, "--", "python3", "-c", probeScript, mode, cfg.BaseURL, cfg.Model, canary)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", mode, err, redact(string(output)))
	}
	return nil
}

func serviceAddresses(ctx context.Context, c client.Client, namespace, name string, servicePort int32) ([]string, int32, error) {
	var service corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &service); err != nil {
		return nil, 0, err
	}
	portName := ""
	found := false
	for _, p := range service.Spec.Ports {
		if p.Port == servicePort && p.Protocol == corev1.ProtocolTCP {
			portName = p.Name
			found = true
		}
	}
	if !found {
		return nil, 0, fmt.Errorf("Service %s has no TCP port %d", name, servicePort)
	}
	ips := append([]string{}, service.Spec.ClusterIPs...)
	var slices discoveryv1.EndpointSliceList
	if err := c.List(ctx, &slices, client.InNamespace(namespace), client.MatchingLabels{discoveryv1.LabelServiceName: name}); err != nil {
		return nil, 0, err
	}
	var port int32
	for _, slice := range slices.Items {
		for _, p := range slice.Ports {
			if p.Port != nil && (p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP) && ((p.Name == nil && portName == "") || (p.Name != nil && *p.Name == portName)) {
				if port != 0 && port != *p.Port {
					return nil, 0, fmt.Errorf("ambiguous endpoint port for %s", name)
				}
				port = *p.Port
			}
		}
		for _, ep := range slice.Endpoints {
			ips = append(ips, ep.Addresses...)
		}
	}
	if len(slices.Items) == 0 || port == 0 {
		return nil, 0, fmt.Errorf("missing endpoints for %s/%s", namespace, name)
	}
	return ips, port, nil
}

// VerifyInference proves that the handoff selector matches exactly the current
// backing Pods of the InferenceService owning the handoff Service.
func (cfg Config) VerifyInference(ctx context.Context, c client.Client) error {
	var service corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: cfg.Service}, &service); err != nil {
		return err
	}
	is, err := inferenceOwner(ctx, c, &service)
	if err != nil {
		return err
	}
	if is == "" {
		return fmt.Errorf("handoff Service has no InferenceService owner")
	}
	selector, err := metav1.LabelSelectorAsSelector(&cfg.Selector)
	if err != nil {
		return err
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(cfg.Namespace)); err != nil {
		return err
	}
	count := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if strings.HasPrefix(pod.Name, "e2e-disconnected-probe-") {
			continue
		}
		owner, err := inferenceOwner(ctx, c, pod)
		if err != nil {
			return err
		}
		matches := selector.Matches(labels.Set(pod.Labels))
		if matches != (owner == is) {
			return fmt.Errorf("inference selector mismatch for Pod %s", pod.Name)
		}
		if matches {
			count++
			if pod.Spec.HostNetwork {
				return fmt.Errorf("inference Pod uses host networking")
			}
		}
	}
	if count == 0 {
		return fmt.Errorf("inference selector matches no backing Pods")
	}
	return nil
}
func inferenceOwner(ctx context.Context, c client.Client, obj client.Object) (types.UID, error) {
	for depth := 0; depth < 10; depth++ {
		var owner *metav1.OwnerReference
		for _, ref := range obj.GetOwnerReferences() {
			if ref.Controller != nil && *ref.Controller {
				copy := ref
				owner = &copy
				break
			}
		}
		if owner == nil {
			return "", nil
		}
		if owner.Kind == "InferenceService" && strings.HasPrefix(owner.APIVersion, "serving.kserve.io/") {
			return owner.UID, nil
		}
		gvk := schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind)
		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: owner.Name}, parent); err != nil {
			return "", err
		}
		if parent.GetUID() != owner.UID {
			return "", fmt.Errorf("stale owner reference on %s", obj.GetName())
		}
		obj = parent
	}
	return "", fmt.Errorf("owner chain too deep")
}

func redact(value string) string {
	for _, name := range []string{"VLLM_API_KEY", "HUGGING_FACE_HUB_TOKEN"} {
		if key := os.Getenv(name); key != "" {
			value = strings.ReplaceAll(value, key, "[REDACTED]")
		}
	}
	if data, err := os.ReadFile(os.Getenv("E2E_PROVIDER_KEY_PATH")); err == nil {
		if key := strings.TrimSpace(string(data)); key != "" {
			value = strings.ReplaceAll(value, key, "[REDACTED]")
		}
	}
	return value
}
func (cfg Config) diagnostics(t *testing.T, c client.Client, namespace string) {
	dir := os.Getenv("ARTIFACT_DIR")
	if dir == "" {
		return
	}
	dir = filepath.Join(dir, "disconnected")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Log(err)
		return
	}
	operatorCtx, operatorCancel := context.WithTimeout(context.Background(), 30*time.Second)
	operatorOutput, _ := exec.CommandContext(operatorCtx, "oc", "logs", "deployment/controller-manager", "-n", namespace, "--tail=500").CombinedOutput()
	operatorCancel()
	_ = os.WriteFile(filepath.Join(dir, "operator.log"), []byte(redact(string(operatorOutput))), 0600)
	for _, ns := range []string{namespace, cfg.Namespace} {
		for _, resource := range []string{"networkpolicies", "events", "pods", "inferenceservices.serving.kserve.io", "servingruntimes.serving.kserve.io"} {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			output, err := exec.CommandContext(ctx, "oc", "get", resource, "-n", ns, "-o", "yaml").CombinedOutput()
			cancel()
			if err != nil {
				t.Logf("diagnostic %s: %v", resource, err)
			}
			_ = os.WriteFile(filepath.Join(dir, ns+"-"+resource+".yaml"), []byte(redact(string(output))), 0600)
		}
		var pods corev1.PodList
		if err := c.List(context.Background(), &pods, client.InNamespace(ns)); err != nil {
			t.Log(err)
			continue
		}
		selector, _ := metav1.LabelSelectorAsSelector(&cfg.Selector)
		for _, pod := range pods.Items {
			if pod.Labels[RunLabel] == "" && (ns != cfg.Namespace || !selector.Matches(labels.Set(pod.Labels))) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			output, _ := exec.CommandContext(ctx, "oc", "logs", "-n", ns, pod.Name, "--all-containers", "--tail=500").CombinedOutput()
			cancel()
			_ = os.WriteFile(filepath.Join(dir, ns+"-"+pod.Name+".log"), []byte(redact(string(output))), 0600)
		}
	}
	// Record no Secret objects or secret values.
	data, _ := json.Marshal(cfg.Selector)
	_ = os.WriteFile(filepath.Join(dir, "inference-selector.json"), data, 0600)
}
