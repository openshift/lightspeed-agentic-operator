package agenticrun

import (
	"context"
	"encoding/json"
	"fmt"

	"time"

	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
	"github.com/openshift/lightspeed-agentic-operator/pkg/configuration"
)

const (
	sandboxModeSandboxClaim = "sandbox-claim"

	errBuildPodSpec         = "build pod spec"
	errCreatePod            = "create pod for"
	errDeletePod            = "delete pod"
	errEnsureAgentTemplate  = "ensure agent template"
	errCreateSandboxClaim   = "failed to create SandboxClaim for"
	errDeleteSandboxClaim   = "failed to delete SandboxClaim"
	errCreateInputConfigMap = "create input ConfigMap"

	errCreateSandbox = "create sandbox"

	sandboxDeletionTimeout = 2 * time.Minute
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch

var smClaimGVK = schema.GroupVersionKind{
	Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxClaim",
}

var smSandboxGVK = schema.GroupVersionKind{
	Group: "agents.x-k8s.io", Version: "v1beta1", Kind: "Sandbox",
}

// SandboxManager manages sandbox lifecycle: create, wait-ready, release.
// Internally it decides between bare-pod and sandbox-claim mode based on
// the configuration cache, builds the PodSpec via PodSpecBuilder, and
// delegates to the appropriate creation path.
type SandboxManager struct {
	client  client.Client
	config  *configuration.Cache
	builder *PodSpecBuilder
	audit   AuditLogger

	namespace       string
	deletionTimeout time.Duration
}

func NewSandboxManager(c client.Client, config *configuration.Cache, namespace string, audit AuditLogger) *SandboxManager {
	return &SandboxManager{
		client:    c,
		config:    config,
		builder:   &PodSpecBuilder{},
		audit:     audit,
		namespace: namespace,
	}
}

// Create handles full sandbox setup: builds input ConfigMap, determines
// SA + RBAC from step, creates the pod/claim, and sets owner refs.
// Returns the resource name used for Release.
func (m *SandboxManager) Create(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	step string,
	agent *agenticv1alpha1.Agent,
	llm *agenticv1alpha1.LLMProvider,
	tools *agenticv1alpha1.ToolsSpec,
	deadline time.Duration,
	query string,
	agentCtx *agentContext,
) (name string, retErr error) {
	if m.audit != nil {
		ctx = m.audit.BeginStep(ctx, run, step)
		if step == "analysis" {
			m.audit.EmitAgenticRunReceived(ctx, run)
		}
		defer func() {
			if retErr != nil {
				m.audit.CompleteStep(run, step, nil)
			}
		}()
	}

	cfg := m.config.Get()
	if cfg == nil {
		return "", fmt.Errorf("%s: configuration not available", errCreateSandbox)
	}

	span := trace.SpanFromContext(ctx)

	serviceAccount := sandboxSAName(run, step)
	if err := m.ensureSA(ctx, run, serviceAccount, step); err != nil {
		return "", err
	}
	span.AddEvent("sandbox.sa.created")

	// After this point, K8s resources are created. If any subsequent step
	// fails before the pod exists, clean up to avoid orphans.
	var createErr error
	defer func() {
		if createErr != nil {
			m.cleanupOnCreateFailure(ctx, run, step, serviceAccount)
		}
	}()

	if step == "execution" && agentCtx != nil && agentCtx.ApprovedOption != nil {
		rbac := &agentCtx.ApprovedOption.RBAC
		if len(rbac.NamespaceScoped) > 0 || len(rbac.ClusterScoped) > 0 {
			base := run.DeepCopy()
			if err := ensureExecutionRBAC(ctx, m.client, run, rbac, m.namespace); err != nil {
				createErr = err
				return "", err
			}
			if err := m.client.Patch(ctx, run, client.MergeFrom(base)); err != nil {
				createErr = fmt.Errorf("persist RBAC annotation: %w", err)
				return "", createErr
			}
		}
	}

	schema := outputSchemaForStep(step, run)
	inputCM, err := buildInputConfigMap(m.namespace, run, step, query, schema, agentCtx)
	if err != nil {
		createErr = err
		return "", err
	}
	if err := m.createInputConfigMap(ctx, inputCM); err != nil {
		createErr = err
		return "", err
	}
	span.AddEvent("sandbox.configmap.created")

	if err := ensureResultRBAC(ctx, m.client, run, step, serviceAccount, m.namespace); err != nil {
		createErr = err
		return "", err
	}
	span.AddEvent("sandbox.rbac.created")

	podSpec, err := m.builder.Build(
		cfg.Sandbox.PodSpec,
		agent,
		llm,
		tools,
		&cfg.OTEL,
		&cfg.RHOKP,
		step,
		string(run.UID),
		serviceAccount,
		inputCM.Name,
		traceparentFromContext(ctx),
	)
	if err != nil {
		createErr = fmt.Errorf("%s: %w", errBuildPodSpec, err)
		return "", createErr
	}

	if deadline > 0 {
		secs := int64(deadline.Seconds())
		podSpec.ActiveDeadlineSeconds = &secs
	}

	name = fmt.Sprintf("ls-%s-%s", step, run.UID)
	var ownerRef metav1.OwnerReference
	if cfg.Sandbox.Mode == sandboxModeSandboxClaim {
		claimName, claimUID, err := m.createSandboxClaim(ctx, run, name, step, podSpec)
		if err != nil {
			createErr = err
			return "", err
		}
		ownerRef = metav1.OwnerReference{
			APIVersion: smClaimGVK.GroupVersion().String(),
			Kind:       smClaimGVK.Kind,
			Name:       claimName,
			UID:        claimUID,
		}
		name = claimName
	} else {
		podName, podUID, err := m.createBarePod(ctx, run, name, step, podSpec)
		if err != nil {
			createErr = err
			return "", err
		}
		ownerRef = metav1.OwnerReference{
			APIVersion: "v1",
			Kind:       "Pod",
			Name:       podName,
			UID:        podUID,
		}
		name = podName
	}

	span.AddEvent("sandbox.pod.created")

	if err := m.setInputConfigMapOwner(ctx, inputConfigMapName(step, string(run.UID)), ownerRef); err != nil {
		return "", err
	}
	if err := setResultRBACOwner(ctx, m.client, string(run.UID), step, ownerRef, m.namespace); err != nil {
		return "", err
	}
	if err := m.setSAOwner(ctx, serviceAccount, ownerRef); err != nil {
		return "", err
	}
	return name, nil
}

// ensureSA creates a per-step ServiceAccount and adds it to the shared reader
// ClusterRoleBindings. Idempotent.
func (m *SandboxManager) ensureSA(ctx context.Context, run *agenticv1alpha1.AgenticRun, saName, step string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: m.namespace,
			Labels:    rbacLabels(string(run.UID), step+"-sa"),
		},
	}
	if err := m.client.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("%s %s: %w", ErrCreateSandboxSA, saName, err)
	}
	return addReaderSubject(ctx, m.client, saName, m.namespace)
}

// setSAOwner sets the pod/claim as owner on the per-run ServiceAccount
// so Kubernetes GC cleans it up when the pod is deleted.
func (m *SandboxManager) setSAOwner(ctx context.Context, saName string, owner metav1.OwnerReference) error {
	sa := &corev1.ServiceAccount{}
	if err := m.client.Get(ctx, client.ObjectKey{Name: saName, Namespace: m.namespace}, sa); err != nil {
		return fmt.Errorf("get sandbox SA %s: %w", saName, err)
	}
	base := sa.DeepCopy()
	sa.OwnerReferences = []metav1.OwnerReference{owner}
	if err := m.client.Patch(ctx, sa, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("set owner on sandbox SA %s: %w", saName, err)
	}
	return nil
}

// cleanupOnCreateFailure removes resources created during Create() before the
// pod existed (ConfigMap, result RBAC, SA). Best-effort: logs errors but does
// not return them — the original creation error takes priority.
func (m *SandboxManager) cleanupOnCreateFailure(ctx context.Context, run *agenticv1alpha1.AgenticRun, step, serviceAccount string) {
	log := logf.FromContext(ctx)
	cmName := inputConfigMapName(step, string(run.UID))
	cm := &corev1.ConfigMap{}
	cm.Name = cmName
	cm.Namespace = m.namespace
	if err := m.client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "cleanup: failed to delete input ConfigMap", LogKeyName, cmName)
	}
	roleName := resultRoleName(string(run.UID), step)
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: m.namespace}}
	if err := m.client.Delete(ctx, role); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "cleanup: failed to delete result Role", LogKeyName, roleName)
	}
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: m.namespace}}
	if err := m.client.Delete(ctx, rb); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "cleanup: failed to delete result RoleBinding", LogKeyName, roleName)
	}
	if step == "execution" {
		if err := cleanupExecutionRBAC(ctx, m.client, run); err != nil {
			log.Error(err, "cleanup: failed to delete execution RBAC")
		}
	}
	if serviceAccount != "" {
		sa := &corev1.ServiceAccount{}
		sa.Name = serviceAccount
		sa.Namespace = m.namespace
		if err := m.client.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "cleanup: failed to delete SA", LogKeyName, serviceAccount)
		}
	}
}

// setInputConfigMapOwner replaces the owner refs on the input ConfigMap with
// the pod/claim owner so Kubernetes GC cleans it up when the pod is deleted.
func (m *SandboxManager) setInputConfigMapOwner(ctx context.Context, cmName string, owner metav1.OwnerReference) error {
	cm := &corev1.ConfigMap{}
	if err := m.client.Get(ctx, client.ObjectKey{Name: cmName, Namespace: m.namespace}, cm); err != nil {
		return fmt.Errorf("get input ConfigMap: %w", err)
	}
	base := cm.DeepCopy()
	cm.OwnerReferences = []metav1.OwnerReference{owner}
	if err := m.client.Patch(ctx, cm, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("set owner on input ConfigMap: %w", err)
	}
	return nil
}

func (m *SandboxManager) createInputConfigMap(ctx context.Context, cm *corev1.ConfigMap) error {
	log := logf.FromContext(ctx)
	if err := m.client.Create(ctx, cm); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("%s %q: %w", errCreateInputConfigMap, cm.Name, err)
	}
	log.Info("Created input ConfigMap", LogKeyName, cm.Name)
	return nil
}

func (m *SandboxManager) createBarePod(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	podName string,
	step string,
	podSpec *corev1.PodSpec,
) (string, types.UID, error) {
	log := logf.FromContext(ctx)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.namespace,
			Labels: map[string]string{
				LabelRun:  string(run.UID),
				LabelStep: step,
			},
			Annotations: map[string]string{
				AnnotationRunName: run.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         agenticv1alpha1.GroupVersion.String(),
				Kind:               "AgenticRun",
				Name:               run.Name,
				UID:                run.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: *podSpec,
	}

	if err := m.client.Create(ctx, pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", "", fmt.Errorf("%s %s: %w", errCreatePod, step, err)
		}

		var existing corev1.Pod
		key := types.NamespacedName{Name: podName, Namespace: m.namespace}
		if getErr := m.client.Get(ctx, key, &existing); getErr != nil {
			return "", "", fmt.Errorf("get existing pod %q: %w", podName, getErr)
		}
		if existing.DeletionTimestamp.IsZero() {
			return podName, existing.UID, nil
		}

		log.Info("Waiting for terminating pod to disappear", LogKeyName, podName)
		if err := m.waitForPodDeletion(ctx, key); err != nil {
			return "", "", fmt.Errorf("wait for terminating pod %q: %w", podName, err)
		}
		if err := m.client.Create(ctx, pod); err != nil {
			if apierrors.IsAlreadyExists(err) {
				if getErr := m.client.Get(ctx, key, &existing); getErr != nil {
					return "", "", fmt.Errorf("get existing pod %q: %w", podName, getErr)
				}
				return podName, existing.UID, nil
			}
			return "", "", fmt.Errorf("%s %s: %w", errCreatePod, step, err)
		}
	}

	log.Info("Created bare pod", LogKeyName, podName, LogKeyStep, step)
	return podName, pod.UID, nil
}

func (m *SandboxManager) createSandboxClaim(
	ctx context.Context,
	run *agenticv1alpha1.AgenticRun,
	name string,
	step string,
	podSpec *corev1.PodSpec,
) (string, types.UID, error) {
	log := logf.FromContext(ctx)

	podSpecMap, err := podSpecToUnstructured(podSpec)
	if err != nil {
		return "", "", fmt.Errorf("convert PodSpec to unstructured: %w", err)
	}

	ownerRef := map[string]any{
		"apiVersion":         agenticv1alpha1.GroupVersion.String(),
		"kind":               "AgenticRun",
		"name":               run.Name,
		"uid":                string(run.UID),
		"controller":         true,
		"blockOwnerDeletion": true,
	}

	template := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1beta1",
			"kind":       "SandboxTemplate",
			"metadata": map[string]any{
				"name":      name,
				"namespace": m.namespace,
				"labels": map[string]any{
					LabelRun:  string(run.UID),
					LabelStep: step,
				},
				"ownerReferences": []any{ownerRef},
			},
			"spec": map[string]any{
				"networkPolicyManagement": "Unmanaged",
				"podTemplate": map[string]any{
					"spec": podSpecMap,
				},
			},
		},
	}
	if err := m.client.Create(ctx, template); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", "", fmt.Errorf("%s: %w", errEnsureAgentTemplate, err)
	}

	pool := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1beta1",
			"kind":       "SandboxWarmPool",
			"metadata": map[string]any{
				"name":      name,
				"namespace": m.namespace,
				"labels": map[string]any{
					LabelRun:  string(run.UID),
					LabelStep: step,
				},
				"ownerReferences": []any{ownerRef},
			},
			"spec": map[string]any{
				"replicas": int64(0),
				"sandboxTemplateRef": map[string]any{
					"name": name,
				},
			},
		},
	}
	if err := m.client.Create(ctx, pool); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", "", fmt.Errorf("create SandboxWarmPool for %s: %w", step, err)
	}

	claim := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": smClaimGVK.Group + "/" + smClaimGVK.Version,
			"kind":       smClaimGVK.Kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": m.namespace,
				"labels": map[string]any{
					LabelRun:  string(run.UID),
					LabelStep: step,
				},
				"annotations": map[string]any{
					AnnotationRunName: run.Name,
				},
				"ownerReferences": []any{ownerRef},
			},
			"spec": map[string]any{
				"warmPoolRef": map[string]any{
					"name": name,
				},
				"lifecycle": map[string]any{
					"shutdownPolicy": "Delete",
				},
			},
		},
	}

	if err := m.client.Create(ctx, claim); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(smClaimGVK)
			if getErr := m.client.Get(ctx, types.NamespacedName{Name: name, Namespace: m.namespace}, existing); getErr != nil {
				return "", "", fmt.Errorf("get existing SandboxClaim %q: %w", name, getErr)
			}
			return name, existing.GetUID(), nil
		}
		return "", "", fmt.Errorf("%s %s: %w", errCreateSandboxClaim, step, err)
	}

	log.Info("Created SandboxClaim", LogKeyClaim, name, LogKeyStep, step)
	return name, types.UID(claim.GetUID()), nil
}

func podSpecToUnstructured(podSpec *corev1.PodSpec) (map[string]any, error) {
	raw, err := json.Marshal(podSpec)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Release ends the audit span and deletes the sandbox resource. Both bare-pod
// and sandbox-claim deletion are attempted for TOCTOU safety (config may have
// changed since Create). ConfigMap, result RBAC, and SA are cleaned up
// automatically via owner references. Cross-namespace execution RBAC (Roles,
// ClusterRoles) and reader subject bindings are cleaned up explicitly.
// Idempotent.
func (m *SandboxManager) Release(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string) error {
	if m.audit != nil {
		m.audit.CompleteStep(run, step, nil)
	}

	claimName := sandboxClaimName(run, step)
	if claimName == "" {
		return nil
	}

	var firstErr error
	if err := m.releaseBarePod(ctx, claimName); err != nil {
		firstErr = err
	}
	if cfg := m.config.Get(); cfg != nil && cfg.Sandbox.Mode == sandboxModeSandboxClaim {
		if err := m.releaseSandboxClaim(ctx, claimName); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	saName := sandboxSAName(run, step)
	if err := removeReaderSubject(ctx, m.client, saName, m.namespace); err != nil && firstErr == nil {
		firstErr = err
	}

	if step == "execution" {
		if err := cleanupExecutionRBAC(ctx, m.client, run); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *SandboxManager) releaseBarePod(ctx context.Context, podName string) error {
	log := logf.FromContext(ctx)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.namespace,
		},
	}

	if err := m.client.Delete(ctx, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s %q: %w", errDeletePod, podName, err)
	}

	log.Info("Released bare pod", LogKeyName, podName)
	return nil
}

func (m *SandboxManager) releaseSandboxClaim(ctx context.Context, claimName string) error {
	log := logf.FromContext(ctx)

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(smClaimGVK)
	claim.SetName(claimName)
	claim.SetNamespace(m.namespace)

	if err := m.client.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %q: %w", errDeleteSandboxClaim, claimName, err)
	}

	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxWarmPool",
	})
	pool.SetName(claimName)
	pool.SetNamespace(m.namespace)

	if err := m.client.Delete(ctx, pool); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete SandboxWarmPool %q: %w", claimName, err)
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxTemplate",
	})
	tmpl.SetName(claimName)
	tmpl.SetNamespace(m.namespace)

	if err := m.client.Delete(ctx, tmpl); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete SandboxTemplate %q: %w", claimName, err)
	}

	log.Info("Released SandboxClaim, SandboxWarmPool, and SandboxTemplate", LogKeyClaim, claimName)
	return nil
}

func (m *SandboxManager) waitForPodDeletion(ctx context.Context, key types.NamespacedName) error {
	timeout := m.deletionTimeout
	if timeout == 0 {
		timeout = sandboxDeletionTimeout
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		var pod corev1.Pod
		err := m.client.Get(ctx, key, &pod)
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return fmt.Errorf("get pod %q: %w", key.Name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for pod %q to be deleted after %s", key.Name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
