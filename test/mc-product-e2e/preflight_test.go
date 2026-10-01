//go:build mc_product_e2e

package mcproducte2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	hubNamespace     = "openshift-lightspeed"
	managedNamespace = "openshift-lightspeed-managed"
)

type clusters struct {
	hub   client.Client
	spoke client.Client
	name  string
}

// TestPrerequisites checks the existing clusters without changing standing configuration
// selfsubjectaccessreviews are transient authorization queries, not fixtures
func TestPrerequisites(t *testing.T) {
	checkPrerequisites(t)
}

func checkPrerequisites(t *testing.T) clusters {
	t.Helper()

	hubPath := requiredEnv(t, "MC_HUB_KUBECONFIG")
	spokePath := requiredEnv(t, "MC_SPOKE_KUBECONFIG")

	s, err := newClusterScheme()
	if err != nil {
		t.Fatalf("register client scheme: %v", err)
	}

	hub := clusterClient(t, hubPath, s)
	spoke := clusterClient(t, spokePath, s)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var spokeSystem corev1.Namespace
	mustGet(t, ctx, spoke, types.NamespacedName{Name: "kube-system"}, &spokeSystem)
	if spokeSystem.UID == "" {
		t.Fatal("spoke kube-system UID must be nonempty")
	}

	for _, item := range []struct {
		c    client.Client
		name string
	}{
		{hub, hubNamespace}, {spoke, managedNamespace},
	} {
		var ns corev1.Namespace
		mustGet(t, ctx, item.c, types.NamespacedName{Name: item.name}, &ns)
		if ns.Status.Phase != corev1.NamespaceActive {
			t.Fatalf("namespace %s is not Active: %s", item.name, ns.Status.Phase)
		}
	}

	hubConfig := &unstructured.Unstructured{}
	hubConfig.SetGroupVersionKind(hubGVK("HubConfig"))
	mustGet(t, ctx, hub, types.NamespacedName{Name: "cluster"}, hubConfig)
	name, err := discoverSpoke(ctx, hub, spokeSystem.UID, os.Getenv("MC_SPOKE_NAME"), standingClusterUID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("registered spoke: %s", name)

	hubDeployment, err := discoverHubDeployment(ctx, hub)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ready hub Deployment: %s", hubDeployment)
	var config corev1.ConfigMap
	mustGet(t, ctx, hub, types.NamespacedName{Namespace: hubNamespace, Name: "lightspeed-agentic-configuration"}, &config)
	if mode := config.Data["sandbox-mode"]; mode != "" && mode != "bare-pod" {
		t.Fatalf("multicluster E2E requires bare-pod sandbox mode, got %q", mode)
	}
	var podSpec corev1.PodSpec
	if err := json.Unmarshal([]byte(config.Data["sandbox-pod-spec"]), &podSpec); err != nil {
		t.Fatalf("sandbox-pod-spec must be valid JSON: %v", err)
	}
	if len(podSpec.Containers) == 0 {
		t.Fatal("sandbox-pod-spec has no containers")
	}
	for _, container := range podSpec.Containers {
		if container.Image == "" || strings.Contains(strings.ToLower(container.Image), "mock") {
			t.Fatal("sandbox-pod-spec must use real non-mock container images")
		}
	}

	var agent agenticv1alpha1.Agent
	mustGet(t, ctx, hub, types.NamespacedName{Namespace: hubNamespace, Name: "default"}, &agent)
	if agent.Spec.Model == "" || agent.Spec.LLMProvider.Name == "" {
		t.Fatal("Agent/default needs a model and LLM provider")
	}
	var provider agenticv1alpha1.LLMProvider
	mustGet(t, ctx, hub, types.NamespacedName{Name: agent.Spec.LLMProvider.Name}, &provider)
	secretName, err := providerCredentialsName(provider.Spec)
	if err != nil {
		t.Fatalf("Agent/default provider is not configured: %v", err)
	}
	var credentials corev1.Secret
	mustGet(t, ctx, hub, types.NamespacedName{Namespace: hubNamespace, Name: secretName}, &credentials)
	if len(credentials.Data) == 0 {
		t.Fatal("LLM provider credentials Secret is empty")
	}
	var policy agenticv1alpha1.ApprovalPolicy
	mustGet(t, ctx, hub, types.NamespacedName{Name: "cluster"}, &policy)
	for step, mode := range map[agenticv1alpha1.SandboxStep]agenticv1alpha1.ApprovalMode{
		agenticv1alpha1.SandboxStepAnalysis:     agenticv1alpha1.ApprovalModeAutomatic,
		agenticv1alpha1.SandboxStepExecution:    agenticv1alpha1.ApprovalModeAutomatic,
		agenticv1alpha1.SandboxStepVerification: agenticv1alpha1.ApprovalModeAutomatic,
	} {
		found := false
		for _, stage := range policy.Spec.Stages {
			if stage.Name == step && stage.Approval == mode {
				found = true
			}
		}
		if !found {
			t.Fatalf("ApprovalPolicy/cluster requires %s=%s", step, mode)
		}
	}

	// check the actor's privileges before creating test fixtures
	for _, check := range []struct {
		c         client.Client
		group     string
		resource  string
		verb      string
		namespace string
	}{
		{hub, "agentic.openshift.io", "agenticruns", "create", hubNamespace},
		{hub, "agentic.openshift.io", "agenticruns", "get", hubNamespace},
		{hub, "agentic.openshift.io", "agenticruns", "delete", hubNamespace},
		{hub, "agentic.openshift.io", "analysisresults", "get", hubNamespace},
		{hub, "agentic.openshift.io", "executionresults", "get", hubNamespace},
		{hub, "agentic.openshift.io", "verificationresults", "get", hubNamespace},
		{hub, "apps", "deployments", "get", hubNamespace},
		{hub, "apps", "deployments", "list", ""},
		{hub, "", "secrets", "get", hubNamespace},
		{hub, "", "secrets", "list", hubNamespace},
		{hub, "", "secrets", "watch", hubNamespace},
		{hub, "", "pods", "get", hubNamespace},
		{hub, "", "pods", "list", hubNamespace},
		{hub, "", "pods", "watch", hubNamespace},
		{hub, "", "configmaps", "get", hubNamespace},
		{spoke, "authorization.k8s.io", "subjectaccessreviews", "create", ""},
		{spoke, "", "namespaces", "create", ""},
		{spoke, "", "namespaces", "get", ""},
		{spoke, "", "namespaces", "delete", ""},
		{spoke, "", "configmaps", "get", ""},
		{spoke, "", "serviceaccounts", "get", managedNamespace},
		{spoke, "", "serviceaccounts", "list", managedNamespace},
		{spoke, "", "serviceaccounts", "watch", managedNamespace},
		{spoke, "rbac.authorization.k8s.io", "clusterrolebindings", "get", ""},
		{spoke, "rbac.authorization.k8s.io", "clusterrolebindings", "list", ""},
		{spoke, "rbac.authorization.k8s.io", "clusterrolebindings", "watch", ""},
		{spoke, "rbac.authorization.k8s.io", "roles", "get", ""},
		{spoke, "rbac.authorization.k8s.io", "roles", "list", ""},
		{spoke, "rbac.authorization.k8s.io", "roles", "watch", ""},
		{spoke, "rbac.authorization.k8s.io", "rolebindings", "get", ""},
		{spoke, "rbac.authorization.k8s.io", "rolebindings", "list", ""},
		{spoke, "rbac.authorization.k8s.io", "rolebindings", "watch", ""},
	} {
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Group: check.group, Resource: check.resource, Verb: check.verb, Namespace: check.namespace,
			},
		}}
		if err := check.c.Create(ctx, review); err != nil {
			t.Fatalf("check %s %s authorization: %v", check.verb, check.resource, err)
		}
		if !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
			t.Fatalf("actor needs %s on %s in %q (authorization denied or indeterminate)", check.verb, check.resource, check.namespace)
		}
	}

	return clusters{hub: hub, spoke: spoke, name: name}
}

func requiredEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s must be set explicitly (no current-context fallback)", key)
	}
	return value
}

func clusterClient(t *testing.T, path string, s *runtime.Scheme) client.Client {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("load explicit kubeconfig: %v", err)
	}
	server, err := url.Parse(cfg.Host)
	if err != nil || server.Scheme != "https" || server.Host == "" || cfg.TLSClientConfig.Insecure {
		t.Fatal("kubeconfig must have a verified HTTPS API server")
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("create cluster client: %v", err)
	}
	return c
}

// choose a registration by the actual cluster it reaches, not by list order
func discoverSpoke(ctx context.Context, hub client.Client, spokeUID types.UID, selected string, identify func(context.Context, []byte) (types.UID, error)) (string, error) {
	var registrations []unstructured.Unstructured
	if selected != "" {
		registration := &unstructured.Unstructured{}
		registration.SetGroupVersionKind(hubGVK("SpokeCluster"))
		if err := hub.Get(ctx, types.NamespacedName{Name: selected}, registration); err != nil {
			return "", fmt.Errorf("get SpokeCluster/%s: %w", selected, err)
		}
		registrations = append(registrations, *registration)
	} else {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(hubGVK("SpokeClusterList"))
		if err := hub.List(ctx, list); err != nil {
			return "", fmt.Errorf("list SpokeClusters for discovery: %w", err)
		}
		registrations = list.Items
	}

	var matches []string
	selectedReady := false
	for _, registration := range registrations {
		if !spokeReady(&registration) {
			continue
		}
		selectedReady = true
		name := registration.GetName()
		var standing corev1.Secret
		if err := hub.Get(ctx, types.NamespacedName{Namespace: hubNamespace, Name: "spoke-kubeconfig-" + name}, &standing); err != nil {
			return "", fmt.Errorf("get standing kubeconfig for SpokeCluster/%s: %w", name, err)
		}
		uid, err := identify(ctx, standing.Data["kubeconfig"])
		if err != nil || uid == "" {
			return "", fmt.Errorf("cannot verify standing kubeconfig for SpokeCluster/%s against supplied spoke cluster", name)
		}
		if uid == spokeUID {
			matches = append(matches, name)
		}
	}
	if len(matches) == 0 {
		if selected != "" && selectedReady {
			return "", fmt.Errorf("SpokeCluster/%s does not match the supplied spoke kubeconfig", selected)
		}
		return "", fmt.Errorf("no ready SpokeCluster matches the supplied spoke kubeconfig")
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple ready SpokeClusters match the supplied spoke kubeconfig; set MC_SPOKE_NAME to choose one")
	}
	return matches[0], nil
}

func spokeReady(registration *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(registration.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, required := range []string{"Connected", "Provisioned", "AdaptersReady", "Ready"} {
		ready := false
		for _, raw := range conditions {
			condition, ok := raw.(map[string]interface{})
			if ok && condition["type"] == required && condition["status"] == "True" {
				ready = true
			}
		}
		if !ready {
			return false
		}
	}
	return true
}

func standingClusterUID(ctx context.Context, data []byte) (types.UID, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return "", fmt.Errorf("standing kubeconfig cannot build a verified TLS client")
	}
	server, err := url.Parse(cfg.Host)
	if err != nil || server.Scheme != "https" || server.Host == "" || cfg.Insecure {
		return "", fmt.Errorf("standing kubeconfig must have a verified HTTPS API server")
	}
	if cfg.ExecProvider != nil || cfg.AuthProvider != nil {
		return "", fmt.Errorf("standing kubeconfig must not run local credential plugins")
	}
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("standing kubeconfig cannot create a spoke client")
	}
	ns, err := api.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("standing kubeconfig cannot read the spoke identity")
	}
	return ns.UID, nil
}

func newClusterScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, authorizationv1.AddToScheme,
		rbacv1.AddToScheme, agenticv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func discoverHubDeployment(ctx context.Context, c client.Client) (types.NamespacedName, error) {
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.MatchingLabels{"app.kubernetes.io/name": "lightspeed-hub"}); err != nil {
		return types.NamespacedName{}, fmt.Errorf("list hub Deployments by product label: %w", err)
	}
	if len(deployments.Items) == 0 {
		return types.NamespacedName{}, fmt.Errorf("no hub Deployment with app.kubernetes.io/name=lightspeed-hub found; label the hub operator Deployment")
	}
	if len(deployments.Items) != 1 {
		return types.NamespacedName{}, fmt.Errorf("multiple hub Deployments with app.kubernetes.io/name=lightspeed-hub; expected exactly one, got %d", len(deployments.Items))
	}
	dep := &deployments.Items[0]
	ref := types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name}
	if !deploymentReady(dep) {
		return types.NamespacedName{}, fmt.Errorf("hub Deployment %s is not ready", ref)
	}
	return ref, nil
}

func deploymentReady(dep *appsv1.Deployment) bool {
	return dep.Spec.Replicas != nil && *dep.Spec.Replicas > 0 && dep.Status.ObservedGeneration >= dep.Generation && dep.Status.ReadyReplicas >= *dep.Spec.Replicas
}

func providerCredentialsName(spec agenticv1alpha1.LLMProviderSpec) (string, error) {
	var name string
	switch spec.Type {
	case agenticv1alpha1.LLMProviderAnthropic:
		name = spec.Anthropic.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		name = spec.GoogleCloudVertex.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderOpenAI:
		name = spec.OpenAI.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		name = spec.AzureOpenAI.CredentialsSecret.Name
	case agenticv1alpha1.LLMProviderAWSBedrock:
		name = spec.AWSBedrock.CredentialsSecret.Name
	default:
		return "", fmt.Errorf("unsupported LLM provider type %q", spec.Type)
	}
	if name == "" {
		return "", fmt.Errorf("LLM provider type %q has no credentials Secret reference", spec.Type)
	}
	return name, nil
}

func hubGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "hub.openshift.io", Version: "v1alpha1", Kind: kind}
}

func mustGet(t *testing.T, ctx context.Context, c client.Client, key types.NamespacedName, obj client.Object) {
	t.Helper()
	if err := c.Get(ctx, key, obj); err != nil {
		t.Fatalf("get %T %s: %v", obj, key, err)
	}
}
