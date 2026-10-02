package disconnected

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestArchiveRunRedactsCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARTIFACT_DIR", dir)
	t.Setenv("VLLM_API_KEY", "test-sensitive-key")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "oc"), []byte("#!/bin/sh\necho test-sensitive-key\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ArchiveRun(t, "agentic", "run", "uid")
	data, err := os.ReadFile(filepath.Join(dir, "disconnected", "runs", "run", "agenticrun.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "test-sensitive-key") || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatal("credentials not redacted")
	}
}

func TestEndpointDiscoveryIgnoresMetricsPort(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = discoveryv1.AddToScheme(s)
	dns, metrics := "dns-tcp", "metrics"
	dnsPort, metricsPort := int32(5353), int32(9154)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "dns-default", Namespace: "openshift-dns"}, Spec: corev1.ServiceSpec{ClusterIPs: []string{"172.30.0.10"}, Ports: []corev1.ServicePort{{Name: dns, Protocol: corev1.ProtocolTCP, Port: 53}}}}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: svc.Namespace, Labels: map[string]string{discoveryv1.LabelServiceName: svc.Name}}, Ports: []discoveryv1.EndpointPort{{Name: &dns, Port: &dnsPort}, {Name: &metrics, Port: &metricsPort}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(svc, slice).Build()
	ips, port, err := serviceAddresses(context.Background(), c, svc.Namespace, svc.Name, 53)
	if err != nil || port != 5353 || len(ips) != 2 {
		t.Fatalf("got %v, %d, %v", ips, port, err)
	}
}
func TestInferenceSelectorExactBackingSet(t *testing.T) {
	cfg := validConfig()
	yes := true
	owner := metav1.OwnerReference{APIVersion: "serving.kserve.io/v1beta1", Kind: "InferenceService", Name: "model", UID: "model-uid", Controller: &yes}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: cfg.Service, Namespace: cfg.Namespace, OwnerReferences: []metav1.OwnerReference{owner}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vllm", Namespace: cfg.Namespace, Labels: map[string]string{"app": "vllm"}, OwnerReferences: []metav1.OwnerReference{owner}}}
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(svc, pod).Build()
	if err := cfg.VerifyInference(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	unrelated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: cfg.Namespace, Labels: pod.Labels}}
	if err := c.Create(context.Background(), unrelated); err != nil {
		t.Fatal(err)
	}
	if cfg.VerifyInference(context.Background(), c) == nil {
		t.Fatal("selector includes unrelated Pod")
	}
	if err := c.Delete(context.Background(), unrelated); err != nil {
		t.Fatal(err)
	}
	pod.Labels = map[string]string{"app": "other"}
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if cfg.VerifyInference(context.Background(), c) == nil {
		t.Fatal("selector omits backing Pod")
	}
}
func TestProbeBaselinePinsOnlyReachableIPs(t *testing.T) {
	harness := `import os, sys, tempfile, socket, ssl, http.client, urllib.request, json
os.chdir(tempfile.mkdtemp())
script = sys.argv[1]
sys.argv = ['probe', 'baseline', 'http://vllm.models.svc:8000/v1', 'model', 'https://example.com/']
socket.getaddrinfo = lambda *a, **k: [(None,None,None,None,('10.0.0.1',443)),(None,None,None,None,('10.0.0.2',443))]
class Conn:
    status = 200
    def __enter__(self): return self
    def __exit__(self,*a): pass
    def sendall(self,*a): pass
    def begin(self): pass
    def close(self): pass
    def wrap_socket(self,*a,**k): return self
ssl.create_default_context = lambda *a, **k: Conn()
def connect(address, **kwargs):
    if address[0] == '10.0.0.2': raise TimeoutError()
    return Conn()
socket.create_connection = connect
http.client.HTTPResponse = lambda *a, **k: Conn()
urllib.request.urlopen = lambda *a, **k: Conn()
exec(script)
assert json.load(open('e2e-canary.json')) == ['10.0.0.1'], 'baseline included an unreachable IP'
`
	// Avoid sharing the real probe's absolute /tmp path during local unit tests.
	script := strings.ReplaceAll(probeScript, "/tmp/e2e-canary.json", "e2e-canary.json")
	output, err := exec.Command("python3", "-c", harness, script).CombinedOutput()
	if err != nil {
		t.Fatalf("baseline probe: %v: %s", err, output)
	}
}
