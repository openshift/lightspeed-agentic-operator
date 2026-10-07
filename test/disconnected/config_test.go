package disconnected

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func validConfig() Config {
	return Config{BaseURL: "http://vllm.models.svc:8000/v1", Model: "google/gemma-4-31B-it", Namespace: "models", Service: "vllm", ServicePort: 8000, NetworkPort: 8080, Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "vllm"}}, Registry: "image-registry.openshift-image-registry.svc:5000"}
}
func TestConfigValidation(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Config){
		"public URL":       func(c *Config) { c.BaseURL = "https://example.com/v1" },
		"wrong service":    func(c *Config) { c.BaseURL = "http://other.models.svc:8000/v1" },
		"no API root":      func(c *Config) { c.BaseURL = "http://vllm.models.svc:8000" },
		"empty selector":   func(c *Config) { c.Selector = metav1.LabelSelector{} },
		"invalid selector": func(c *Config) { c.Selector.MatchLabels = map[string]string{"invalid/key/key": "x"} },
		"zero port":        func(c *Config) { c.NetworkPort = 0 },
		"no model":         func(c *Config) { c.Model = "" },
		"public registry":  func(c *Config) { c.Registry = "quay.io" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			change(&c)
			if c.Validate() == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}
func TestLocalImages(t *testing.T) {
	c := validConfig()
	local := c.Registry + "/tests/skills:latest"
	c.Images = map[string]string{"quay.io/skills:v1": local}
	for _, source := range []string{"quay.io/skills:v1", local} {
		got, err := c.LocalImage(source)
		if err != nil || got != local {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	for _, source := range []string{"quay.io/missing:v1", c.Registry + ".evil/skills:v1", "", c.Registry + "/../skills:v1"} {
		if _, err := c.LocalImage(source); err == nil {
			t.Fatalf("accepted %q", source)
		}
	}
	c.Images["quay.io/skills:v1"] = "quay.io/not-mirrored:v1"
	if _, err := c.LocalImage("quay.io/skills:v1"); err == nil {
		t.Fatal("accepted external mapping")
	}
}
func TestValidatePod(t *testing.T) {
	c := validConfig()
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{RunLabel: "uid"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: c.Registry + "/test/sandbox:v1"}}}}
	if err := c.ValidatePod(&p); err != nil {
		t.Fatal(err)
	}
	p.Labels = nil
	if c.ValidatePod(&p) == nil {
		t.Fatal("accepted unselected sandbox")
	}
	p.Labels = map[string]string{RunLabel: "uid"}
	p.Spec.InitContainers = []corev1.Container{{Image: "quay.io/external:v1"}}
	if c.ValidatePod(&p) == nil {
		t.Fatal("accepted external init image")
	}
}
func TestPolicies(t *testing.T) {
	c := validConfig()
	policies, err := c.Policies("agentic", []string{"172.30.0.10", "10.0.0.10"}, []string{"172.30.0.1", "10.0.0.1"}, 5353, 6443)
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 2 {
		t.Fatal("want sandbox and inference policies")
	}
	sandbox, inference := policies[0], policies[1]
	if sandbox.Spec.PodSelector.MatchExpressions[0].Key != RunLabel {
		t.Fatal("sandbox selector must use stable run label")
	}
	if inference.Spec.PodSelector.MatchLabels["app"] != "vllm" {
		t.Fatal("lost inference selector")
	}
	if len(sandbox.Spec.Egress) != 3 || len(inference.Spec.Egress) != 2 {
		t.Fatal("unexpected permissions")
	}
	for _, policy := range policies {
		for _, rule := range policy.Spec.Egress {
			for _, peer := range rule.To {
				if peer.IPBlock != nil && peer.IPBlock.CIDR[len(peer.IPBlock.CIDR)-3:] != "/32" {
					t.Fatalf("broad peer %s", peer.IPBlock.CIDR)
				}
			}
		}
	}
	if _, err := c.Policies("agentic", []string{"0.0.0.0/0"}, []string{"10.0.0.1"}, 5353, 6443); err == nil {
		t.Fatal("accepted broad CIDR")
	}
	if _, err := c.Policies("agentic", nil, []string{"10.0.0.1"}, 5353, 6443); err == nil {
		t.Fatal("accepted missing DNS peers")
	}
}
