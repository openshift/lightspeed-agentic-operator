/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agenticv1alpha1 "github.com/openshift/lightspeed-agentic-operator/api/v1alpha1"
)

const (
	requeueWhenNotReady = 30 * time.Second

	reasonReady                 = "Ready"
	reasonLLMProviderMissing    = "LLMProviderMissing"
	reasonLLMProviderNotFound   = "LLMProviderNotFound"
	reasonLLMProviderInvalid    = "LLMProviderInvalid"
	reasonCredentialsSecretName = "CredentialsSecretMissing"
	reasonSecretNotFound        = "SecretNotFound"
	reasonSecretKeysMissing     = "SecretKeysMissing"
)

const (
	ErrGetLLMProvider = "get LLMProvider"
	ErrGetSecret      = "get Secret"
	ErrPatchStatus    = "patch Agent status"
)

// Reconciler sets Agent status.conditions[type=Ready] and status.ready
// from the referenced LLMProvider and credentials Secret.
type Reconciler struct {
	client.Client
	Namespace string
}

// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentic.openshift.io,resources=llmproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var agent agenticv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	status, reason, message, err := r.evaluate(ctx, &agent)
	if err != nil {
		return ctrl.Result{}, err
	}

	ready := string(status)
	existing := meta.FindStatusCondition(agent.Status.Conditions, agenticv1alpha1.AgentConditionReady)
	if existing != nil &&
		existing.Status == status &&
		existing.Reason == reason &&
		existing.Message == message &&
		existing.ObservedGeneration == agent.Generation &&
		agent.Status.Ready == ready {
		if status != metav1.ConditionTrue {
			return ctrl.Result{RequeueAfter: requeueWhenNotReady}, nil
		}
		return ctrl.Result{}, nil
	}

	base := agent.DeepCopy()
	meta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:               agenticv1alpha1.AgentConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: agent.Generation,
	})
	agent.Status.Ready = ready
	if err := r.Status().Patch(ctx, &agent, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("%s: %w", ErrPatchStatus, err)
	}
	if status != metav1.ConditionTrue {
		return ctrl.Result{RequeueAfter: requeueWhenNotReady}, nil
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) evaluate(ctx context.Context, agent *agenticv1alpha1.Agent) (metav1.ConditionStatus, string, string, error) {
	providerName := agent.Spec.LLMProvider.Name
	if providerName == "" {
		return metav1.ConditionFalse, reasonLLMProviderMissing, "Agent.spec.llmProvider.name is empty", nil
	}

	var llm agenticv1alpha1.LLMProvider
	if err := r.Get(ctx, types.NamespacedName{Name: providerName}, &llm); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse, reasonLLMProviderNotFound,
				fmt.Sprintf("LLMProvider %q not found", providerName), nil
		}
		return "", "", "", fmt.Errorf("%s %q: %w", ErrGetLLMProvider, providerName, err)
	}

	secretName, err := credentialsSecretName(&llm)
	if err != nil {
		return metav1.ConditionFalse, reasonLLMProviderInvalid, err.Error(), nil
	}
	if secretName == "" {
		return metav1.ConditionFalse, reasonCredentialsSecretName,
			fmt.Sprintf("LLMProvider %q has no credentialsSecret.name", providerName), nil
	}

	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: r.Namespace}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse, reasonSecretNotFound,
				fmt.Sprintf("Secret %q not found in namespace %q", secretName, r.Namespace), nil
		}
		return "", "", "", fmt.Errorf("%s %s/%s: %w", ErrGetSecret, r.Namespace, secretName, err)
	}

	if msg := missingCredentialKeys(&llm, &secret); msg != "" {
		return metav1.ConditionFalse, reasonSecretKeysMissing, msg, nil
	}

	return metav1.ConditionTrue, reasonReady,
		fmt.Sprintf("LLMProvider %q and Secret %q are accessible", providerName, secretName), nil
}

func credentialsSecretName(llm *agenticv1alpha1.LLMProvider) (string, error) {
	switch llm.Spec.Type {
	case agenticv1alpha1.LLMProviderAnthropic:
		return llm.Spec.Anthropic.CredentialsSecret.Name, nil
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		return llm.Spec.GoogleCloudVertex.CredentialsSecret.Name, nil
	case agenticv1alpha1.LLMProviderOpenAI:
		return llm.Spec.OpenAI.CredentialsSecret.Name, nil
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		return llm.Spec.AzureOpenAI.CredentialsSecret.Name, nil
	case agenticv1alpha1.LLMProviderAWSBedrock:
		return llm.Spec.AWSBedrock.CredentialsSecret.Name, nil
	default:
		return "", fmt.Errorf("LLMProvider %q has unknown type %q", llm.Name, llm.Spec.Type)
	}
}

func missingCredentialKeys(llm *agenticv1alpha1.LLMProvider, secret *corev1.Secret) string {
	has := func(key string) bool {
		v, ok := secret.Data[key]
		return ok && len(v) > 0
	}
	switch llm.Spec.Type {
	case agenticv1alpha1.LLMProviderAnthropic:
		if !has("ANTHROPIC_API_KEY") {
			return fmt.Sprintf("Secret %q is missing key ANTHROPIC_API_KEY", secret.Name)
		}
	case agenticv1alpha1.LLMProviderGoogleCloudVertex:
		if !has("GOOGLE_APPLICATION_CREDENTIALS") {
			return fmt.Sprintf("Secret %q is missing key GOOGLE_APPLICATION_CREDENTIALS", secret.Name)
		}
	case agenticv1alpha1.LLMProviderOpenAI:
		if !has("OPENAI_API_KEY") {
			return fmt.Sprintf("Secret %q is missing key OPENAI_API_KEY", secret.Name)
		}
	case agenticv1alpha1.LLMProviderAzureOpenAI:
		if has("apitoken") {
			return ""
		}
		if has("client_id") && has("tenant_id") && has("client_secret") {
			return ""
		}
		return fmt.Sprintf("Secret %q must contain apitoken or client_id+tenant_id+client_secret", secret.Name)
	case agenticv1alpha1.LLMProviderAWSBedrock:
		var missing []string
		if !has("AWS_ACCESS_KEY_ID") {
			missing = append(missing, "AWS_ACCESS_KEY_ID")
		}
		if !has("AWS_SECRET_ACCESS_KEY") {
			missing = append(missing, "AWS_SECRET_ACCESS_KEY")
		}
		if len(missing) > 0 {
			return fmt.Sprintf("Secret %q is missing keys %v", secret.Name, missing)
		}
	}
	return ""
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agenticv1alpha1.Agent{}).
		Watches(
			&agenticv1alpha1.LLMProvider{},
			handler.EnqueueRequestsFromMapFunc(r.mapLLMProvider),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecret),
		).
		Named("agent").
		Complete(r)
}

func (r *Reconciler) mapLLMProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.requestsForProvider(ctx, obj.GetName())
}

func (r *Reconciler) mapSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Namespace {
		return nil
	}
	var providers agenticv1alpha1.LLMProviderList
	if err := r.List(ctx, &providers); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	seen := map[string]bool{}
	for i := range providers.Items {
		name, err := credentialsSecretName(&providers.Items[i])
		if err != nil || name != obj.GetName() {
			continue
		}
		for _, req := range r.requestsForProvider(ctx, providers.Items[i].Name) {
			if seen[req.Name] {
				continue
			}
			seen[req.Name] = true
			reqs = append(reqs, req)
		}
	}
	return reqs
}

func (r *Reconciler) requestsForProvider(ctx context.Context, providerName string) []reconcile.Request {
	var agents agenticv1alpha1.AgentList
	if err := r.List(ctx, &agents); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range agents.Items {
		if agents.Items[i].Spec.LLMProvider.Name != providerName {
			continue
		}
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: agents.Items[i].Name},
		})
	}
	return reqs
}
