package ocpversion

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type failingReader struct{ client.Reader }

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("forbidden")
}

type allowHandler struct{}

func (allowHandler) Handle(_ context.Context, _ admission.Request) admission.Response {
	return admission.Allowed("ok")
}

func TestLiveActivationAndAdmission(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	cv := Object()
	cv.SetName("version")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cv).Build()
	gate := &Gate{Reader: c}
	admissionGate := Admission{Gate: gate, Next: allowHandler{}}
	check := func(want bool) {
		t.Helper()
		if got := gate.Enabled(ctx); got != want {
			t.Fatalf("Enabled() = %v, want %v", got, want)
		}
		if got := admissionGate.Handle(ctx, admission.Request{}).Allowed; got != want {
			t.Fatalf("admission allowed = %v, want %v", got, want)
		}
	}
	check(false) // fresh install with an unreported version
	// During an upgrade, Desired.Version is already 5.0 before the rollout
	// completes. Agentic must not start while that history entry is Partial.
	partial := Object()
	if err := c.Get(ctx, client.ObjectKey{Name: "version"}, partial); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(partial.Object, "5.0.0", "status", "desired", "version"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(partial.Object, []interface{}{map[string]interface{}{"state": "Partial", "version": "5.0.0"}}, "status", "history"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, partial); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := c.Get(ctx, client.ObjectKey{Name: "version"}, partial); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(partial.Object, []interface{}{map[string]interface{}{"state": "Completed", "version": "5.0.0"}}, "status", "history"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, partial); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err := c.Get(ctx, client.ObjectKey{Name: "version"}, partial); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		version string
		history []interface{}
		enabled bool
	}{
		{"4.22.0", []interface{}{map[string]interface{}{"state": "Completed", "version": "4.22.0"}}, false},
		{"5.0.0", []interface{}{map[string]interface{}{"state": "Completed", "version": "5.0.0"}}, true},
		{"5.1.0", []interface{}{map[string]interface{}{"state": "Completed", "version": "5.1.0"}}, true},
		{"5.1.0", []interface{}{map[string]interface{}{"state": "Completed", "version": "5.0.0"}}, false},
		{"5.1.0", []interface{}{map[string]interface{}{"state": "Partial", "version": "5.1.0"}}, false},
		{"5.0.0", nil, false},
		{"5.0.0", []interface{}{}, false},
		{"5.0.0", []interface{}{map[string]interface{}{"state": "Completed"}}, false},
		{"invalid", []interface{}{map[string]interface{}{"state": "Completed", "version": "invalid"}}, false},
		{"5.not-a-minor", []interface{}{map[string]interface{}{"state": "Completed", "version": "5.not-a-minor"}}, false},
		{"", []interface{}{map[string]interface{}{"state": "Completed", "version": ""}}, false},
	} {
		obj := Object()
		if err := c.Get(ctx, client.ObjectKey{Name: "version"}, obj); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(obj.Object, tc.version, "status", "desired", "version"); err != nil {
			t.Fatal(err)
		}
		if tc.history == nil {
			delete(obj.Object["status"].(map[string]interface{}), "history")
		} else if err := unstructured.SetNestedSlice(obj.Object, tc.history, "status", "history"); err != nil {
			t.Fatal(err)
		}
		if err := c.Update(ctx, obj); err != nil {
			t.Fatal(err)
		}
		check(tc.enabled)
	}
	if (&Gate{Reader: failingReader{}}).Enabled(ctx) {
		t.Fatal("unreadable ClusterVersion activated agentic work")
	}
	if enabled, err := (&Gate{Reader: failingReader{}}).Check(ctx); enabled || err == nil {
		t.Fatalf("read failure must fail closed and be retried: enabled=%v, err=%v", enabled, err)
	}
	if enabled, err := gate.Check(ctx); enabled || err != nil {
		t.Fatalf("readable unsupported release should not retry: enabled=%v, err=%v", enabled, err)
	}
	if err := c.Delete(ctx, cv); err != nil {
		t.Fatal(err)
	}
	check(false)
}
