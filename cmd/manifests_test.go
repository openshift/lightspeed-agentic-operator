package main

import (
	"io"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func manifest(t *testing.T, path, kind string) *unstructured.Unstructured {
	t.Helper()
	f, err := os.Open("../config/" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				t.Fatalf("%s not found in %s", kind, path)
			}
			t.Fatal(err)
		}
		if obj.GetKind() == kind {
			return obj
		}
	}
}

func TestMetricsDeployment(t *testing.T) {
	deployment := manifest(t, "manager/manager.yaml", "Deployment")
	containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]interface{})
	args, _, _ := unstructured.NestedStringSlice(container, "args")
	for _, want := range []string{"--leader-elect", "--secure-metrics-server", "--metrics-bind-address=:8443", "--cert-dir=/etc/tls/private"} {
		found := false
		for _, arg := range args {
			found = found || arg == want
		}
		if !found {
			t.Errorf("deployment missing %s", want)
		}
	}
	service := manifest(t, "metrics/service.yaml", "Service")
	if service.GetAnnotations()["service.beta.openshift.io/serving-cert-secret-name"] != "agentic-operator-metrics-certs" {
		t.Fatal("metrics Service must request a service-ca certificate")
	}
	volumes, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "volumes")
	found := false
	for _, volume := range volumes {
		name, _, _ := unstructured.NestedString(volume.(map[string]interface{}), "secret", "secretName")
		found = found || name == "agentic-operator-metrics-certs"
	}
	if !found {
		t.Fatal("deployment must mount the service-ca Secret")
	}
}

func TestPrometheusDiscoveryRBAC(t *testing.T) {
	role := manifest(t, "prometheus/role.yaml", "Role")
	rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
	if len(rules) == 0 {
		t.Fatal("Prometheus discovery requires namespace-scoped read permissions")
	}
	binding := manifest(t, "prometheus/role_binding.yaml", "RoleBinding")
	subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
	if len(subjects) != 1 {
		t.Fatal("Prometheus discovery RoleBinding must have one ServiceAccount subject")
	}
	subject := subjects[0].(map[string]interface{})
	if subject["kind"] != "ServiceAccount" || subject["name"] != "prometheus-k8s" || subject["namespace"] != "openshift-monitoring" {
		t.Fatal("discovery permissions must target the cluster Prometheus ServiceAccount")
	}
}

func TestMetricsScraping(t *testing.T) {
	monitor := manifest(t, "prometheus/monitor.yaml", "ServiceMonitor")
	endpoints, _, _ := unstructured.NestedSlice(monitor.Object, "spec", "endpoints")
	endpoint := endpoints[0].(map[string]interface{})
	if endpoint["scheme"] != "https" || endpoint["port"] != "metrics" {
		t.Fatal("monitor must scrape the named metrics port over HTTPS")
	}
	for key, want := range map[string]string{
		"certFile": "/etc/prometheus/secrets/metrics-client-certs/tls.crt",
		"keyFile":  "/etc/prometheus/secrets/metrics-client-certs/tls.key",
	} {
		got, _, _ := unstructured.NestedString(endpoint, "tlsConfig", key)
		if got != want {
			t.Errorf("monitor %s = %q, want %q", key, got, want)
		}
	}
	serverName, _, _ := unstructured.NestedString(endpoint, "tlsConfig", "serverName")
	if serverName == "" {
		t.Fatal("monitor must verify the Service DNS name when scraping pod IPs")
	}
	manifest(t, "metrics/networkpolicy.yaml", "NetworkPolicy")
	role := manifest(t, "rbac/leader_election_role.yaml", "Role")
	rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
	if len(rules) == 0 {
		t.Fatal("leader election requires namespaced RBAC")
	}
}
