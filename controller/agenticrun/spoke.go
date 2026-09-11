package agenticrun

import (
	"context"
	"fmt"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	spokeKubeconfigPrefix = "spoke-kubeconfig-"
	spokeManagedNamespace = "openshift-lightspeed-managed"
	kubeconfigKey         = "kubeconfig"

	spokeDialTimeout = 10 * time.Second

	ErrGetSpokeKubeconfig   = "get spoke kubeconfig Secret"
	ErrParseSpokeKubeconfig = "parse spoke kubeconfig"
	ErrCreateSpokeClient    = "create spoke client"
	ErrRequestSpokeToken    = "request spoke SA token"

	// spokeTokenExpiration is the lifetime of per-step SA tokens on spoke.
	// 24h per spec — safety net if cleanup fails.
	spokeTokenExpiration = int64(86400)

	// Labels for spoke-created resources — auditability and manual cleanup sweeps.
	LabelSpokeCluster = "hub.openshift.io/spoke-cluster"
	LabelAgenticRun   = "hub.openshift.io/agentic-run"
)

// SpokeAccess holds everything needed to operate on a spoke cluster.
// Nil when the AgenticRun targets the local (hub) cluster.
type SpokeAccess struct {
	Client    client.Client
	Config    *rest.Config
	Namespace string // always spokeManagedNamespace
}

// spokeLabels returns labels for resources created on the spoke cluster.
// Includes the standard run/component labels plus spoke-specific labels
// for cross-cluster auditability.
func spokeLabels(runUID, runName, targetCluster, component string) map[string]string {
	return map[string]string{
		LabelRun:          runUID,
		LabelComponent:    component,
		LabelSpokeCluster: targetCluster,
		LabelAgenticRun:   runName,
	}
}

// NewClientFromConfig creates a controller-runtime client from a rest.Config.
// Exported as a variable so tests can inject a fake client.
var NewClientFromConfig = func(cfg *rest.Config) (client.Client, error) {
	return client.New(cfg, client.Options{})
}

// spokeAccessForRun returns a SpokeAccess for the run's targetCluster.
// Returns nil when targetCluster is empty (local cluster — caller uses
// hub client and operator namespace, unchanged behavior).
func spokeAccessForRun(ctx context.Context, hubClient client.Client, run *agenticv1alpha1.AgenticRun, operatorNS string) (*SpokeAccess, error) {
	tc := run.Spec.TargetCluster
	if tc == "" {
		return nil, nil
	}

	secretName := spokeKubeconfigPrefix + tc
	secret := &corev1.Secret{}
	if err := hubClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: operatorNS}, secret); err != nil {
		return nil, fmt.Errorf("%s %s: %w", ErrGetSpokeKubeconfig, secretName, err)
	}

	kubeconfigBytes, ok := secret.Data[kubeconfigKey]
	if !ok {
		return nil, fmt.Errorf("%s %s: missing %q key", ErrParseSpokeKubeconfig, secretName, kubeconfigKey)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", ErrParseSpokeKubeconfig, secretName, err)
	}
	cfg.Timeout = spokeDialTimeout

	spokeClient, err := NewClientFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%s for spoke %s: %w", ErrCreateSpokeClient, tc, err)
	}

	return &SpokeAccess{
		Client:    spokeClient,
		Config:    cfg,
		Namespace: spokeManagedNamespace,
	}, nil
}

// NewClientsetFromConfig creates a kubernetes.Interface from a rest.Config.
// Exported as a variable so tests can inject a fake clientset.
var NewClientsetFromConfig = func(cfg *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(cfg)
}

// requestSpokeToken calls the TokenRequest API on the spoke for a per-step SA.
// Returns a 24h token string. Empty audience, no bound object ref (per design
// decisions #2 and #3).
//
// Intentionally unused until OLS-3951 wires it into sandbox kubeconfig creation.
// Kept here (with tests) so OLS-3951 only needs to add the call site.
func requestSpokeToken(ctx context.Context, cfg *rest.Config, saName, spokeNS string) (string, error) {
	clientset, err := NewClientsetFromConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("%s: create clientset: %w", ErrRequestSpokeToken, err)
	}

	expiration := spokeTokenExpiration
	treq := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &expiration,
		},
	}

	result, err := clientset.CoreV1().ServiceAccounts(spokeNS).CreateToken(ctx, saName, treq, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("%s %s/%s: %w", ErrRequestSpokeToken, spokeNS, saName, err)
	}

	return result.Status.Token, nil
}
