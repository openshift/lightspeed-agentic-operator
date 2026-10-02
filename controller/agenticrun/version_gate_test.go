package agenticrun

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/lightspeed-agentic-operator/pkg/ocpversion"
)

type unavailableVersionReader struct{ client.Reader }

func (unavailableVersionReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("ClusterVersion API temporarily unavailable")
}

func TestVersionGateSkipsRunOnFourAndEnablesOnCompletedUpgrade(t *testing.T) {
	ctx := context.Background()
	run := testAgenticRun()
	cv := ocpversion.Object()
	cv.SetName("version")
	cv.Object["status"] = map[string]interface{}{
		"desired": map[string]interface{}{"version": "4.22.0"},
		"history": []interface{}{map[string]interface{}{"state": "Completed", "version": "4.22.0"}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(run, cv).Build()
	caller := newTestAgentCaller()
	r := &AgenticRunReconciler{Client: c, Agent: caller, Version: &ocpversion.Gate{Reader: c}}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if caller.releaseAllCount != 0 {
		t.Fatal("inactive reconcile performed sandbox work")
	}
	got := testAgenticRun()
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 0 || len(got.Status.Conditions) != 0 {
		t.Fatal("inactive gate mutated the existing run")
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "version"}, cv); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(cv.Object, "5.0.0", "status", "desired", "version"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(cv.Object, []interface{}{map[string]interface{}{"state": "Partial", "version": "5.0.0"}}, "status", "history"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, cv); err != nil {
		t.Fatal(err)
	}
	if r.Version.Enabled(ctx) {
		t.Fatal("partial upgrade activated agentic work")
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "version"}, cv); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(cv.Object, []interface{}{map[string]interface{}{"state": "Completed", "version": "5.0.0"}}, "status", "history"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, cv); err != nil {
		t.Fatal(err)
	}
	if !r.Version.Enabled(ctx) {
		t.Fatal("completed upgrade did not activate agentic work")
	}
	// The completion event must be retryable even if its direct read fails once.
	r.Version = &ocpversion.Gate{Reader: unavailableVersionReader{}}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("transient read failure lost the completed upgrade event")
	}
	r.Version = &ocpversion.Gate{Reader: c}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("expected missing handoff configuration after version recovery")
	}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) == 0 {
		t.Fatal("run did not resume reconciliation after version read recovered")
	}
}
