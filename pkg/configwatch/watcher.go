package configwatch

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
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
// Returning an error fails WaitFor (startup). Reconcile logs handler errors
// and does not requeue — invalid config is not fixed by retrying.
type Handler func(ctx context.Context, cm *corev1.ConfigMap) error

// Registration binds a ConfigMap name to a handler.
type Registration struct {
	Name    string
	Handler Handler
}

// Watcher is a controller-runtime reconciler that watches specific ConfigMaps
// by name and dispatches to registered handlers via informers.
type Watcher struct {
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

// SetupWithManager registers the watcher as a controller that watches ConfigMaps.
func (w *Watcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("configmap-watcher").
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
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(w)
}

// WaitFor blocks until the named ConfigMap exists or the timeout expires,
// then invokes the handler with the found ConfigMap.
// Used at startup before the manager is running (informers not yet active).
// Handler errors are returned to the caller (fatal at startup).
func WaitFor(ctx context.Context, c client.Reader, namespace, name string, timeout time.Duration, h Handler) error {
	log := logf.FromContext(ctx)
	log.Info("Waiting for ConfigMap", "name", name, "timeout", timeout)

	var found *corev1.ConfigMap
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, cm); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return false, nil
			}
			return false, fmt.Errorf("%s %q: %w", ErrGetConfigMap, name, err)
		}
		found = cm
		return true, nil
	})
	if err != nil {
		if wait.Interrupted(err) {
			return fmt.Errorf("%s %q after %s: %w", ErrWaitConfigMap, name, timeout, err)
		}
		return err
	}

	log.Info("ConfigMap found", "name", name)
	return h(ctx, found)
}
