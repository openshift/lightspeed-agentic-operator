package ocpversion

import (
	"context"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Admission rejects writes while the cluster version is not positively >= 5.0.
// In particular a mutating webhook must not return Allowed without a patch.
type Admission struct {
	Gate *Gate
	Next admission.Handler
}

func (a Admission) Handle(ctx context.Context, req admission.Request) admission.Response {
	if !a.Gate.Enabled(ctx) {
		return admission.Denied("agentic operations require a readable OpenShift version >= 5.0")
	}
	return a.Next.Handle(ctx, req)
}
