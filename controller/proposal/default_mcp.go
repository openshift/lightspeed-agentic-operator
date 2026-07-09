package proposal

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

var olsConfigGVK = schema.GroupVersionKind{
	Group: "ols.openshift.io", Version: "v1alpha1", Kind: "OLSConfig",
}

const (
	defaultMCPServerName = "openshift"
	defaultMCPServerPort = 8080
	olsConfigName        = "cluster"
)

// +kubebuilder:rbac:groups=ols.openshift.io,resources=olsconfigs,verbs=get;watch

// readIntrospectionEnabled reads the classic OLSConfig CR and returns
// the value of spec.olsConfig.introspectionEnabled (defaults to true).
func readIntrospectionEnabled(ctx context.Context, c client.Client) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(olsConfigGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: olsConfigName}, obj); err != nil {
		return false, fmt.Errorf("get OLSConfig %q: %w", olsConfigName, err)
	}

	val, found, err := unstructured.NestedBool(obj.Object, "spec", "olsConfig", "introspectionEnabled")
	if err != nil {
		return true, nil
	}
	if !found {
		return true, nil
	}
	return val, nil
}

// defaultMCPServer builds an MCPServerConfig entry for the built-in
// ocp-mcp sidecar Service created by the classic operator.
func defaultMCPServer(namespace string) agenticv1alpha1.MCPServerConfig {
	return agenticv1alpha1.MCPServerConfig{
		Name:           defaultMCPServerName,
		URL:            fmt.Sprintf("http://openshift-mcp-server.%s.svc:%d/mcp", namespace, defaultMCPServerPort),
		TimeoutSeconds: 60,
		Headers: []agenticv1alpha1.MCPHeader{
			{
				Name: "Authorization",
				ValueFrom: agenticv1alpha1.MCPHeaderValueSource{
					Type: agenticv1alpha1.MCPHeaderSourceTypeServiceAccountToken,
				},
			},
		},
	}
}

// effectiveTools returns a copy of the given ToolsSpec with the default
// ocp-mcp server prepended when introspection is enabled and the Proposal
// has not opted out via defaultMCP: Disabled. Returns the original pointer
// unmodified when no injection is needed.
func effectiveTools(ctx context.Context, c client.Client, namespace string, tools *agenticv1alpha1.ToolsSpec) *agenticv1alpha1.ToolsSpec {
	log := logf.FromContext(ctx).WithName("default-mcp")

	if tools != nil && tools.DefaultMCP == agenticv1alpha1.DefaultMCPModeDisabled {
		return tools
	}

	enabled, err := readIntrospectionEnabled(ctx, c)
	if err != nil {
		log.V(1).Info("Cannot read OLSConfig, skipping default MCP injection", "error", err)
		return tools
	}
	if !enabled {
		return tools
	}

	server := defaultMCPServer(namespace)

	if tools == nil {
		return &agenticv1alpha1.ToolsSpec{
			MCPServers: []agenticv1alpha1.MCPServerConfig{server},
		}
	}

	merged := tools.DeepCopy()
	merged.MCPServers = append([]agenticv1alpha1.MCPServerConfig{server}, merged.MCPServers...)
	return merged
}
