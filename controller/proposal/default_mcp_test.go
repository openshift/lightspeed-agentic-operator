package proposal

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

func olsConfigUnstructured(introspectionEnabled *bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(olsConfigGVK)
	obj.SetName(olsConfigName)
	spec := map[string]any{"olsConfig": map[string]any{}}
	if introspectionEnabled != nil {
		spec["olsConfig"].(map[string]any)["introspectionEnabled"] = *introspectionEnabled
	}
	obj.Object["spec"] = spec
	return obj
}

func boolPtr(b bool) *bool { return &b }

func TestEffectiveTools_IntrospectionEnabled(t *testing.T) {
	scheme := runtime.NewScheme()
	config := olsConfigUnstructured(boolPtr(true))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()

	result := effectiveTools(context.Background(), fc, "openshift-lightspeed", nil)
	if result == nil {
		t.Fatal("expected non-nil ToolsSpec")
	}
	if len(result.MCPServers) != 1 {
		t.Fatalf("expected 1 MCP server, got %d", len(result.MCPServers))
	}
	if result.MCPServers[0].Name != defaultMCPServerName {
		t.Errorf("expected server name %q, got %q", defaultMCPServerName, result.MCPServers[0].Name)
	}
	if result.MCPServers[0].URL != "http://openshift-mcp-server.openshift-lightspeed.svc:8080/mcp" {
		t.Errorf("unexpected URL: %s", result.MCPServers[0].URL)
	}
}

func TestEffectiveTools_IntrospectionDisabled(t *testing.T) {
	scheme := runtime.NewScheme()
	config := olsConfigUnstructured(boolPtr(false))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()

	result := effectiveTools(context.Background(), fc, "openshift-lightspeed", nil)
	if result != nil {
		t.Fatalf("expected nil ToolsSpec when introspection disabled, got %+v", result)
	}
}

func TestEffectiveTools_IntrospectionOmitted_DefaultsTrue(t *testing.T) {
	scheme := runtime.NewScheme()
	config := olsConfigUnstructured(nil)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()

	result := effectiveTools(context.Background(), fc, "openshift-lightspeed", nil)
	if result == nil {
		t.Fatal("expected non-nil ToolsSpec when introspectionEnabled is omitted (defaults to true)")
	}
	if len(result.MCPServers) != 1 {
		t.Fatalf("expected 1 MCP server, got %d", len(result.MCPServers))
	}
}

func TestEffectiveTools_DisableDefaultMCP(t *testing.T) {
	scheme := runtime.NewScheme()
	config := olsConfigUnstructured(boolPtr(true))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()

	tools := &agenticv1alpha1.ToolsSpec{DisableDefaultMCP: true}
	result := effectiveTools(context.Background(), fc, "openshift-lightspeed", tools)
	if result != tools {
		t.Error("expected original tools pointer returned when DisableDefaultMCP is true")
	}
}

func TestEffectiveTools_PrependToExisting(t *testing.T) {
	scheme := runtime.NewScheme()
	config := olsConfigUnstructured(boolPtr(true))
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()

	existing := &agenticv1alpha1.ToolsSpec{
		MCPServers: []agenticv1alpha1.MCPServerConfig{
			{Name: "custom", URL: "http://custom:9090/mcp"},
		},
	}
	result := effectiveTools(context.Background(), fc, "openshift-lightspeed", existing)
	if len(result.MCPServers) != 2 {
		t.Fatalf("expected 2 MCP servers (default + custom), got %d", len(result.MCPServers))
	}
	if result.MCPServers[0].Name != defaultMCPServerName {
		t.Errorf("default should be first, got %q", result.MCPServers[0].Name)
	}
	if result.MCPServers[1].Name != "custom" {
		t.Errorf("custom should be second, got %q", result.MCPServers[1].Name)
	}
	if len(existing.MCPServers) != 1 {
		t.Error("original tools must not be mutated")
	}
}

func TestEffectiveTools_OLSConfigNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	result := effectiveTools(context.Background(), fc, "openshift-lightspeed", nil)
	if result != nil {
		t.Error("expected nil (passthrough) when OLSConfig cannot be read")
	}
}

func TestDefaultMCPServer(t *testing.T) {
	server := defaultMCPServer("my-namespace")
	if server.Name != defaultMCPServerName {
		t.Errorf("unexpected name: %s", server.Name)
	}
	if server.URL != "http://openshift-mcp-server.my-namespace.svc:8080/mcp" {
		t.Errorf("unexpected URL: %s", server.URL)
	}
	if len(server.Headers) != 1 {
		t.Fatalf("expected 1 header, got %d", len(server.Headers))
	}
	if server.Headers[0].ValueFrom.Type != agenticv1alpha1.MCPHeaderSourceTypeServiceAccountToken {
		t.Errorf("expected ServiceAccountToken header, got %s", server.Headers[0].ValueFrom.Type)
	}
}
