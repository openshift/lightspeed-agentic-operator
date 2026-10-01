package agenticolsconfig

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/lightspeed-agentic-operator/pkg/ocpversion"
)

type unavailableVersionReader struct{ client.Reader }

func (unavailableVersionReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("ClusterVersion API temporarily unavailable")
}

func TestReconcileRetriesTransientVersionReadFailure(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(testConfig(false)).Build()
	r := &Reconciler{Client: c, Version: &ocpversion.Gate{Reader: unavailableVersionReader{}}}
	if _, err := reconcileOnce(r); err == nil {
		t.Fatal("completed upgrade event was lost on a transient version read failure")
	}
}
