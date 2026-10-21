package configwatch

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Handler is called when a watched ConfigMap is created, updated, or deleted.
// Receives nil when the ConfigMap is deleted.
// Reconcile logs handler errors and does not requeue: invalid configuration
// is not fixed by retrying.
type Handler func(ctx context.Context, cm *corev1.ConfigMap) error

// Registration binds a ConfigMap name to a handler.
type Registration struct {
	Name    string
	Handler Handler
}

// Watcher is a controller-runtime reconciler that watches specific ConfigMaps
// by name and dispatches to registered handlers via informers.
type Watcher struct {
	// Name distinguishes independent watchers; empty uses configmap-watcher.
	Name      string
	client    client.Client
	namespace string
	handlers  map[string]Handler
}

// New creates a Watcher with the given registrations.
func New(c client.Client, namespace string, registrations ...Registration) *Watcher {
	handlers := make(map[string]Handler, len(registrations))
	for _, r := range registrations {
		handlers[r.Name] = r.Handler
	}
	return &Watcher{
		client:    c,
		namespace: namespace,
		handlers:  handlers,
	}
}

// Reconcile handles ConfigMap events and dispatches to the appropriate handler.
func (w *Watcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != w.namespace {
		return ctrl.Result{}, nil
	}

	h, ok := w.handlers[req.Name]
	if !ok {
		return ctrl.Result{}, nil
	}

	log := logf.FromContext(ctx)
	cm := &corev1.ConfigMap{}
	if err := w.client.Get(ctx, req.NamespacedName, cm); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ConfigMap deleted, notifying handler", "name", req.Name)
			if err := h(ctx, nil); err != nil {
				log.Error(err, "ConfigMap delete handler failed", "name", req.Name)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.V(1).Info("ConfigMap changed, notifying handler", "name", req.Name)
	if err := h(ctx, cm); err != nil {
		// Invalid config is not fixed by requeue; handler already applied
		// its fallback (e.g. disable telemetry).
		log.Error(err, "ConfigMap handler failed", "name", req.Name)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers a ConfigMap watcher on every replica so standby
// configuration stays current before the replica acquires leadership.
func (w *Watcher) SetupWithManager(mgr ctrl.Manager) error {
	name := w.Name
	if name == "" {
		name = "configmap-watcher"
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WatchesRawSource(source.Kind(mgr.GetCache(), &corev1.ConfigMap{},
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, cm *corev1.ConfigMap) []reconcile.Request {
				if cm.Namespace != w.namespace {
					return nil
				}
				if _, ok := w.handlers[cm.Name]; !ok {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}}}
			}),
		)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1, NeedLeaderElection: ptr.To(false)}).
		Complete(w)
}
