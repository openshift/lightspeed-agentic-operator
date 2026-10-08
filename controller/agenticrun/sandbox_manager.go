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

	// Resolve spoke access once for the entire Create flow.
	// nil when targetCluster is empty (hub path — unchanged behavior).
	spoke, err := spokeAccessForRun(ctx, m.client, run, m.namespace)
	if err != nil {
		return "", err
	}

	serviceAccount := sandboxSAName(run, step)

	// Register cleanup so that any failure after SA creation cleans up
	// spoke resources, RBAC, and the SA itself.
	var createErr error
	defer func() {
		if createErr != nil {
			cleanupCtx, cancel := cleanupContext(ctx)
			defer cancel()
			m.cleanupOnCreateFailure(cleanupCtx, run, step, serviceAccount, spoke)
		}
	}()

	token, err := m.ensureSA(ctx, run, serviceAccount, step, spoke)
	if err != nil {
		createErr = err
		return "", err
	}

	// For spoke runs, the sandbox pod uses the default SA with automount
	// enabled — the automounted token provides hub API access for writing
	// Result CRs. Spoke cluster access comes via a mounted kubeconfig
	// (KUBECONFIG env var). The two credential paths don't conflict.
	podServiceAccount := serviceAccount
	if spoke != nil {
		podServiceAccount = "default"
	}

	if spoke != nil {
		if err := m.ensureSandboxKubeconfig(ctx, run, step, spoke, token); err != nil {
			createErr = err
			return "", err
		}
	}
	span.AddEvent("sandbox.sa.created")

	if step == "execution" && agentCtx != nil && agentCtx.ApprovedOption != nil {
		rbac := &agentCtx.ApprovedOption.RBAC
		if len(rbac.NamespaceScoped) > 0 || len(rbac.ClusterScoped) > 0 {
			// Execution RBAC targets spoke when targetCluster is set.
			rbacClient := m.client
			rbacNS := m.namespace
			var extraLabels map[string]string
			if spoke != nil {
				rbacClient = spoke.Client
				rbacNS = spoke.Namespace
				extraLabels = spokeExtraLabels(run.Name, run.Spec.TargetCluster)
			}
			base := run.DeepCopy()
			if err := ensureExecutionRBAC(ctx, rbacClient, run, rbac, rbacNS, extraLabels); err != nil {
				createErr = err
				return "", err
			}
			// Annotation is persisted on hub (run lives on hub).
			if err := m.client.Patch(ctx, run, client.MergeFrom(base)); err != nil {
				createErr = fmt.Errorf("persist RBAC annotation: %w", err)
				return "", createErr
			}
		}
	}

	schema := outputSchemaForStep(step, run)
	inputCM, err := buildInputConfigMap(m.namespace, run, step, agent, schema, agentCtx)
	if err != nil {
		createErr = err
		return "", err
	}
	if err := m.createInputConfigMap(ctx, inputCM); err != nil {
		createErr = err
		return "", err
	}
	span.AddEvent("sandbox.configmap.created")

	// Result RBAC is always on the hub — the sandbox pod writes its Result CR
	// to the hub API server. Bind to podServiceAccount (= "default" for spoke,
	// = per-step SA for hub) so the pod's actual hub identity has permission.
	if err := ensureResultRBAC(ctx, m.client, run, step, podServiceAccount, m.namespace); err != nil {
		createErr = err
		return "", err
	}
	span.AddEvent("sandbox.rbac.created")

	timeoutSecs := resolveTimeout(agent, step)
	maxTurns := resolveMaxTurns(agent)

	podSpec, err := m.builder.Build(
		cfg.Sandbox.PodSpec,
		agent,
		llm,
		tools,
		cfg,
		step,
		string(run.UID),
		podServiceAccount,
		inputCM.Name,
		traceparentFromContext(ctx),
		timeoutSecs,
		maxTurns,
	)
	if err != nil {
		createErr = fmt.Errorf("%s: %w", errBuildPodSpec, err)
		return "", createErr
	}

	if spoke != nil {
		mountSpokeKubeconfig(podSpec, sandboxKubeconfigSecretName(string(run.UID), step))
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

	// Post-workload owner-ref updates. These do NOT set createErr because
	// the workload is already running — cleanupOnCreateFailure must not
	// delete dependencies from under it. Return name (not "") so the
	// caller can persist it and ReleaseSandboxes can find the workload.
	if err := m.setInputConfigMapOwner(ctx, inputConfigMapName(step, string(run.UID)), ownerRef); err != nil {
		return name, err
	}
	if err := setResultRBACOwner(ctx, m.client, string(run.UID), step, ownerRef, m.namespace); err != nil {
		return name, err
	}
	// Skip SA owner ref for spoke SAs — cross-cluster owner refs don't work.
	// Spoke SA cleanup is explicit via Release/finalizer path.
	if spoke == nil {
		if err := m.setSAOwner(ctx, serviceAccount, ownerRef); err != nil {
			return name, err
		}
	}
	return name, nil
}

// ensureSA creates a per-step ServiceAccount and adds it to reader
// ClusterRoleBindings. For spoke runs, creates the SA on the spoke cluster
// with spoke labels, creates per-run CRBs, and requests a 24h bound token
// via TokenRequest. For hub runs, adds the SA to the shared reader CRBs.
// Idempotent.
//
// Returns the spoke token (non-empty for spoke, empty for hub).
func (m *SandboxManager) ensureSA(ctx context.Context, run *agenticv1alpha1.AgenticRun, saName, step string, spoke *SpokeAccess) (string, error) {
	if spoke != nil {
		// spoke path: SA on spoke with spoke-specific labels
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      saName,
				Namespace: spoke.Namespace,
				Labels:    spokeLabels(string(run.UID), run.Name, run.Spec.TargetCluster, step+"-sa"),
			},
		}
		created := false
		if err := spoke.Client.Create(ctx, sa); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return "", fmt.Errorf("%s %s: %w", ErrCreateSandboxSA, saName, err)
			}
		} else {
			created = true
		}
		if err := addReaderSubjectOnSpoke(ctx, spoke.Client, string(run.UID), step, saName, spoke.Namespace, spokeExtraLabels(run.Name, run.Spec.TargetCluster)); err != nil {
			if created {
				_ = spoke.Client.Delete(ctx, sa)
			}
			return "", err
		}
		// Request a 24h bound token for the spoke SA. This token is embedded
		// in the sandbox kubeconfig Secret so the pod can target the spoke.
		token, err := requestSpokeToken(ctx, spoke.Config, saName, spoke.Namespace)
		if err != nil {
			return "", err
		}
		return token, nil
	}

	// hub path: unchanged
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: m.namespace,
			Labels:    rbacLabels(string(run.UID), step+"-sa"),
		},
	}
	created := false
	if err := m.client.Create(ctx, sa); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("%s %s: %w", ErrCreateSandboxSA, saName, err)
		}
	} else {
		created = true
	}
	if err := addReaderSubject(ctx, m.client, string(run.UID), step, saName, m.namespace); err != nil {
		if created {
			_ = m.client.Delete(ctx, sa)
		}
		return "", err
	}
	return "", nil
}

// ensureSandboxKubeconfig creates or refreshes the hub Secret containing the
// spoke kubeconfig (server URL, CA, proxy-url, ephemeral token). On retry
// (AlreadyExists), the Secret data is refreshed so the pod gets the freshly
// requested token.
func (m *SandboxManager) ensureSandboxKubeconfig(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string, spoke *SpokeAccess, token string) error {
	kubeconfigData, err := buildSandboxKubeconfig(spoke, token)
	if err != nil {
		return err
	}
	kcSecretName := sandboxKubeconfigSecretName(string(run.UID), step)
	kcSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kcSecretName,
			Namespace: m.namespace,
			Labels:    rbacLabels(string(run.UID), "sandbox-kubeconfig"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: agenticv1alpha1.GroupVersion.String(),
				Kind:       "AgenticRun",
				Name:       run.Name,
				UID:        run.UID,
			}},
		},
		Data: map[string][]byte{
			spokeKubeconfigFileName: kubeconfigData,
		},
	}
	if err := m.client.Create(ctx, kcSecret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create sandbox kubeconfig Secret %s: %w", kcSecretName, err)
		}
		// On retry, refresh the Secret data so the pod mounts the
		// freshly requested token instead of a potentially expired one.
		existing := &corev1.Secret{}
		if err := m.client.Get(ctx, client.ObjectKey{Name: kcSecretName, Namespace: m.namespace}, existing); err != nil {
			return fmt.Errorf("get existing sandbox kubeconfig Secret %s: %w", kcSecretName, err)
		}
		// Defense-in-depth: verify the existing Secret belongs to this run.
		if existing.Labels[LabelRun] != string(run.UID) {
			return fmt.Errorf("sandbox kubeconfig Secret %s belongs to a different run (label %s != %s)", kcSecretName, existing.Labels[LabelRun], string(run.UID))
		}
		existing.Data = kcSecret.Data
		if err := m.client.Update(ctx, existing); err != nil {
			return fmt.Errorf("update sandbox kubeconfig Secret %s: %w", kcSecretName, err)
		}
	}
	return nil
}

// mountSpokeKubeconfig adds the spoke kubeconfig Secret as a volume and sets
// the KUBECONFIG env var on all containers in the pod spec.
func mountSpokeKubeconfig(podSpec *corev1.PodSpec, secretName string) {
	kcMountPath := spokeKubeconfigMountPath + "/" + spokeKubeconfigFileName
	podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
		Name: spokeKubeconfigVolume,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
			},
		},
	})
	for i := range podSpec.Containers {
		podSpec.Containers[i].VolumeMounts = append(podSpec.Containers[i].VolumeMounts, corev1.VolumeMount{
			Name:      spokeKubeconfigVolume,
			MountPath: spokeKubeconfigMountPath,
			ReadOnly:  true,
		})
		podSpec.Containers[i].Env = append(podSpec.Containers[i].Env, corev1.EnvVar{
			Name:  "KUBECONFIG",
			Value: kcMountPath,
		})
	}
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
func (m *SandboxManager) cleanupOnCreateFailure(ctx context.Context, run *agenticv1alpha1.AgenticRun, step, serviceAccount string, spoke *SpokeAccess) {
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
	// Spoke kubeconfig Secret cleanup (hub-side, alongside ConfigMap + result RBAC).
	// Check LabelRun before deleting — a colliding name from a different run
	// must not be removed (see ownership guard in Create).
	if spoke != nil {
		kcName := sandboxKubeconfigSecretName(string(run.UID), step)
		existing := &corev1.Secret{}
		if err := m.client.Get(ctx, client.ObjectKey{Name: kcName, Namespace: m.namespace}, existing); err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, "cleanup: failed to get sandbox kubeconfig Secret", LogKeyName, kcName)
			}
		} else if existing.Labels[LabelRun] == string(run.UID) {
			if err := m.client.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "cleanup: failed to delete sandbox kubeconfig Secret", LogKeyName, kcName)
			}
		}
	}
	// SA + reader CRBs + execution RBAC cleanup via shared function.
	if serviceAccount != "" {
		if err := cleanupStepRBAC(ctx, spoke, m.client, m.namespace, run, step); err != nil {
			log.Error(err, "cleanup: step RBAC cleanup", LogKeyName, serviceAccount)
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
// Release ends the audit span and deletes the sandbox resource. spoke is
// pre-resolved by the caller (ReleaseSandbox / ReleaseSandboxes) so that
// bulk teardown reads the kubeconfig Secret only once.
func (m *SandboxManager) Release(ctx context.Context, run *agenticv1alpha1.AgenticRun, step string, spoke *SpokeAccess) error {
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

	// RBAC + SA cleanup via shared function (works for both spoke and hub).
	if err := cleanupStepRBAC(ctx, spoke, m.client, m.namespace, run, step); err != nil && firstErr == nil {
		firstErr = err
	}

	// Delete the sandbox kubeconfig Secret eagerly. Owner-ref GC would
	// clean it up when the AgenticRun is deleted, but the Secret contains
	// a 24h spoke token — no reason to keep it after the step completes.
	// Use TargetCluster (not spoke != nil) so the Secret is deleted even
	// when the spoke is unreachable — the Secret is a hub resource.
	if run.Spec.TargetCluster != "" {
		kcName := sandboxKubeconfigSecretName(string(run.UID), step)
		kcSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: kcName, Namespace: m.namespace}}
		if err := m.client.Delete(ctx, kcSecret); err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
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
