// Package disconnected implements test-only restricted-network product E2E support.
package disconnected

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
)

const RunLabel = "agentic.openshift.io/run"
const OwnedLabel = "agentic.openshift.io/disconnected-e2e"
const InternalRegistry = "image-registry.openshift-image-registry.svc:5000"

// Config is the non-secret OLS-4228 provisioning handoff plus mirrored image inputs.
type Config struct {
	BaseURL, Model, Namespace, Service, Registry string
	ServicePort, NetworkPort                     int32
	Selector                                     metav1.LabelSelector
	Images                                       map[string]string
}

func FromEnv() (Config, error) {
	c := Config{BaseURL: os.Getenv("RHOAI_VLLM_BASE_URL"), Model: os.Getenv("RHOAI_VLLM_MODEL"), Namespace: os.Getenv("RHOAI_VLLM_NAMESPACE"), Service: os.Getenv("RHOAI_VLLM_SERVICE_NAME"), Registry: InternalRegistry}
	for key, dst := range map[string]*int32{"RHOAI_VLLM_SERVICE_PORT": &c.ServicePort, "RHOAI_VLLM_NETWORK_PORT": &c.NetworkPort} {
		n, err := strconv.ParseInt(os.Getenv(key), 10, 32)
		if err != nil {
			return c, fmt.Errorf("invalid %s: %w", key, err)
		}
		*dst = int32(n)
	}
	if err := json.Unmarshal([]byte(os.Getenv("RHOAI_VLLM_POD_SELECTOR_JSON")), &c.Selector); err != nil {
		return c, fmt.Errorf("inference selector: %w", err)
	}
	if path := os.Getenv("E2E_SKILL_IMAGE_MAP"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		if err = json.Unmarshal(data, &c.Images); err != nil {
			return c, fmt.Errorf("skill image map: %w", err)
		}
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.Model == "" || len(validation.IsDNS1123Label(c.Namespace)) != 0 || len(validation.IsDNS1035Label(c.Service)) != 0 {
		return fmt.Errorf("handoff requires model, valid namespace and service")
	}
	if c.ServicePort < 1 || c.ServicePort > 65535 || c.NetworkPort < 1 || c.NetworkPort > 65535 {
		return fmt.Errorf("handoff ports must be in 1..65535")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("invalid vLLM URL: %w", err)
	}
	host := c.Service + "." + c.Namespace + ".svc"
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if (u.Scheme != "http" && u.Scheme != "https") || (u.Hostname() != host && u.Hostname() != host+".cluster.local") || u.Path != "/v1" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || port != strconv.Itoa(int(c.ServicePort)) {
		return fmt.Errorf("vLLM URL must be the handoff Service's internal /v1 endpoint")
	}
	selector, err := metav1.LabelSelectorAsSelector(&c.Selector)
	if err != nil || selector.Empty() {
		return fmt.Errorf("inference selector must be valid and nonempty")
	}
	if c.Registry != InternalRegistry {
		return fmt.Errorf("only the OpenShift cluster-local registry is permitted")
	}
	return nil
}

// LocalImage only maps explicitly mirrored references; runtime Pod validation never rewrites.
func (c Config) LocalImage(source string) (string, error) {
	image := source
	if mapped, ok := c.Images[source]; ok {
		image = mapped
	}
	if err := c.checkImage(image); err != nil {
		return "", err
	}
	return image, nil
}
func (c Config) checkImage(image string) error {
	path := strings.TrimPrefix(image, c.Registry+"/")
	// Require an explicit namespace/repository, with an optional tag or digest.
	valid := regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+(?::[A-Za-z0-9_][A-Za-z0-9_.-]*|@sha256:[a-f0-9]{64})?$`)
	if !strings.HasPrefix(image, c.Registry+"/") || !valid.MatchString(path) {
		return fmt.Errorf("image %q is not a valid cluster-local pullspec; mirror it and supply E2E_SKILL_IMAGE_MAP", image)
	}
	return nil
}
func (c Config) ValidatePod(p *corev1.Pod) error {
	if p.Labels[RunLabel] == "" {
		return fmt.Errorf("sandbox %s lacks %s", p.Name, RunLabel)
	}
	if p.Spec.HostNetwork {
		return fmt.Errorf("sandbox %s uses host networking", p.Name)
	}
	for _, containers := range [][]corev1.Container{p.Spec.Containers, p.Spec.InitContainers} {
		for _, container := range containers {
			if err := c.checkImage(container.Image); err != nil {
				return err
			}
		}
	}
	for _, container := range p.Spec.EphemeralContainers {
		if err := c.checkImage(container.Image); err != nil {
			return err
		}
	}
	for _, v := range p.Spec.Volumes {
		if v.Image != nil {
			if err := c.checkImage(v.Image.Reference); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c Config) Policies(namespace string, dnsIPs, apiIPs []string, dnsPort, apiPort int32) ([]networkingv1.NetworkPolicy, error) {
	peers := func(ips []string) ([]networkingv1.NetworkPolicyPeer, error) {
		if len(ips) == 0 {
			return nil, fmt.Errorf("missing service/endpoint addresses")
		}
		var result []networkingv1.NetworkPolicyPeer
		for _, ip := range ips {
			address := net.ParseIP(ip)
			if address == nil || address.IsUnspecified() || address.IsLoopback() {
				return nil, fmt.Errorf("invalid peer IP %q", ip)
			}
			bits := 32
			if address.To4() == nil {
				bits = 128
			}
			result = append(result, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: fmt.Sprintf("%s/%d", ip, bits)}})
		}
		return result, nil
	}
	dns, err := peers(dnsIPs)
	if err != nil {
		return nil, err
	}
	api, err := peers(apiIPs)
	if err != nil {
		return nil, err
	}
	if apiPort < 1 || apiPort > 65535 || dnsPort < 1 || dnsPort > 65535 {
		return nil, fmt.Errorf("invalid API/DNS endpoint port")
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	port := func(protocol corev1.Protocol, n int32) networkingv1.NetworkPolicyPort {
		p := intstr.FromInt32(n)
		return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &p}
	}
	base := []networkingv1.NetworkPolicyEgressRule{{To: dns, Ports: []networkingv1.NetworkPolicyPort{port(udp, 53), port(tcp, 53), port(udp, dnsPort), port(tcp, dnsPort)}}, {To: api, Ports: []networkingv1.NetworkPolicyPort{port(tcp, 443), port(tcp, apiPort)}}}
	sandbox := networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "e2e-disconnected-sandbox", Namespace: namespace}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: RunLabel, Operator: metav1.LabelSelectorOpExists}}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: append(append([]networkingv1.NetworkPolicyEgressRule{}, base...), networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": c.Namespace}}, PodSelector: &c.Selector}}, Ports: []networkingv1.NetworkPolicyPort{port(tcp, c.NetworkPort)}})}}
	inference := networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "e2e-disconnected-inference", Namespace: c.Namespace}, Spec: networkingv1.NetworkPolicySpec{PodSelector: c.Selector, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: base}}
	return []networkingv1.NetworkPolicy{sandbox, inference}, nil
}
