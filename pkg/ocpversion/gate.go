// Package ocpversion guards agentic work using the live OpenShift ClusterVersion.
// A missing, malformed, or unreadable version is never interpreted as enabled.
package ocpversion

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var GVK = schema.GroupVersionKind{Group: "config.openshift.io", Version: "v1", Kind: "ClusterVersion"}

func Object() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(GVK)
	return obj
}

// Gate uses a direct API reader, not the manager's cached client: stale cached
// versions must not authorize operations after a version change or lookup failure.
type Gate struct{ Reader client.Reader }

func (g *Gate) Enabled(ctx context.Context) bool {
	enabled, _ := g.Check(ctx)
	return enabled
}

// Check distinguishes a retryable API read failure from a readable but
// unsupported, incomplete, or malformed release. Both fail closed.
func (g *Gate) Check(ctx context.Context) (bool, error) {
	if g == nil || g.Reader == nil {
		return true, nil
	} // nil gate is used only by legacy unit fixtures
	obj := Object()
	if err := g.Reader.Get(ctx, client.ObjectKey{Name: "version"}, obj); err != nil {
		return false, fmt.Errorf("read ClusterVersion/version: %w", err)
	}
	version, found, err := unstructured.NestedString(obj.Object, "status", "desired", "version")
	if err != nil || !found {
		return false, nil
	}
	// Status.Desired advances at the *start* of an upgrade. Do not activate
	// during a partial 4.x -> 5.0 rollout, before the new release completes.
	history, found, err := unstructured.NestedSlice(obj.Object, "status", "history")
	if err != nil || !found || len(history) == 0 {
		return false, nil
	}
	entry, ok := history[0].(map[string]interface{})
	if !ok || entry["state"] != "Completed" || entry["version"] != version {
		return false, nil
	}
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false, nil
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false, nil
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return false, nil
	}
	return major >= 5, nil
}
