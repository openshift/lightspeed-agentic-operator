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

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentTimeouts configures per-step and per-turn timeout limits.
// All values are in seconds.
//
// +kubebuilder:validation:MinProperties=1
type AgentTimeouts struct {
	// analysisSeconds is the timeout for the analysis step in seconds.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	AnalysisSeconds int32 `json:"analysisSeconds,omitempty"`

	// executionSeconds is the timeout for the execution step in seconds.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	ExecutionSeconds int32 `json:"executionSeconds,omitempty"`

	// verificationSeconds is the timeout for the verification step in seconds.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	VerificationSeconds int32 `json:"verificationSeconds,omitempty"`

	// chatSeconds is the timeout for each chat turn with the LLM in seconds.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=600
	ChatSeconds int32 `json:"chatSeconds,omitempty"`
}

// StepInstructions holds the system and user prompt instructions for a single step.
type StepInstructions struct {
	// systemPrompt is the LLM system message for this step.
	// Replaces the product built-in system prompt when non-empty.
	// Written to /input/system-prompt in the sandbox.
	// Default (when empty): "You are an AI agent." (sandbox built-in).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32768
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// userPrompt is a Go template that replaces the product built-in query
	// template for this step when non-empty. Supports the same template
	// variables as the built-in (e.g. {{.Request}}, {{.HasExecution}}).
	// Written to /input/query in the sandbox after rendering.
	// Default (when empty): built-in templates in controller/agenticrun/templates/*.tmpl.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32768
	UserPrompt string `json:"userPrompt,omitempty"`
}

// AgentInstructions provides optional per-step system and user instructions.
type AgentInstructions struct {
	// analysis instructions for the analysis step.
	// +optional
	Analysis *StepInstructions `json:"analysis,omitzero"` //nolint:kubeapilinter // all fields optional; empty means use defaults

	// execution instructions for the execution step.
	// +optional
	Execution *StepInstructions `json:"execution,omitzero"` //nolint:kubeapilinter // all fields optional; empty means use defaults

	// verification instructions for the verification step.
	// +optional
	Verification *StepInstructions `json:"verification,omitzero"` //nolint:kubeapilinter // all fields optional; empty means use defaults

	// escalation instructions for the escalation step.
	// +optional
	Escalation *StepInstructions `json:"escalation,omitzero"` //nolint:kubeapilinter // all fields optional; empty means use defaults
}

// AgentSpec defines the desired state of Agent.
type AgentSpec struct {
	// llmProvider references a cluster-scoped LLMProvider CR that supplies the
	// LLM backend for this agent tier.
	// +required
	LLMProvider LLMProviderReference `json:"llmProvider,omitzero"`

	// model is the LLM model identifier as recognized by the provider
	// (e.g., "claude-opus-4-6", "claude-haiku-4-5", "gpt-4o").
	// Must start with an alphanumeric character and may contain
	// alphanumerics, dots, hyphens, underscores, slashes, colons,
	// and at-signs. Maximum 256 characters.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="self.matches('^[a-zA-Z0-9][a-zA-Z0-9._\\\\-/:@]*$')",message="model must start with an alphanumeric character and contain only alphanumerics, dots, hyphens, underscores, slashes, colons, and at-signs"
	Model string `json:"model,omitempty"`

	// timeouts configures per-step and per-turn timeout limits.
	// When omitted, the agent sandbox uses its built-in defaults.
	// +optional
	Timeouts AgentTimeouts `json:"timeouts,omitzero"`

	// maxTurns is the maximum number of tool-use turns the agent may take
	// in a single step invocation. Prevents runaway loops.
	// When omitted, the agent sandbox uses its built-in default.
	// Minimum 1, maximum 500.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	MaxTurns int32 `json:"maxTurns,omitempty"`

	// instructions provides optional per-step system and user instructions.
	// When a field is non-empty it fully replaces the product built-in
	// instructions for that step. Omitted or empty fields fall back to
	// built-in defaults.
	// +optional
	Instructions *AgentInstructions `json:"instructions,omitzero"` //nolint:kubeapilinter // all fields optional; empty means use defaults

	// reasoningConfig is a freeform map of provider- and model-specific
	// reasoning parameters. The exact keys and values depend on the provider
	// and model — consult the provider's SDK documentation for supported
	// parameters (e.g., Claude: thinking/budget_tokens, Gemini: thinking_budget/
	// thinking_level, OpenAI: reasoning.effort/reasoning.summary).
	// The operator serializes this map as the LIGHTSPEED_REASONING_CONFIG JSON
	// env var on the sandbox pod without validation — the sandbox and upstream
	// SDK/API validate at invocation time. Invalid keys are ignored by the
	// adapter; invalid values on recognized keys are rejected by the SDK/API.
	// When omitted, the env var is not set and the sandbox uses SDK defaults.
	// +optional
	// +kubebuilder:validation:MinProperties=1
	ReasoningConfig map[string]apiextensionsv1.JSON `json:"reasoningConfig,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="LLM",type=string,JSONPath=`.spec.llmProvider.name`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent defines a cluster-scoped agent tier (e.g., "default", "smart", "fast").
// The cluster admin creates Agent resources to configure LLM infrastructure
// and runtime settings. AgenticRuns reference agents by name per step.
//
// Agent is cluster-scoped. The metadata.name serves as the tier identifier.
// The "default" agent must exist; "smart" and "fast" are optional (the
// operator auto-links to "default" if absent).
//
// Example — a high-capability agent tier with extended thinking:
//
//	apiVersion: agentic.openshift.io/v1alpha1
//	kind: Agent
//	metadata:
//	  name: smart
//	spec:
//	  llmProvider:
//	    name: vertex-ai
//	  model: claude-opus-4-6
//	  timeouts:
//	    analysisSeconds: 300
//	    executionSeconds: 600
//	  maxTurns: 200
//	  reasoningConfig:
//	    thinking: "enabled"
//	    effort: "high"
//
// Example — a fast, cost-efficient agent tier:
//
//	apiVersion: agentic.openshift.io/v1alpha1
//	kind: Agent
//	metadata:
//	  name: fast
//	spec:
//	  llmProvider:
//	    name: vertex-ai
//	  model: claude-haiku-4-5
//	  timeouts:
//	    analysisSeconds: 120
//	    executionSeconds: 300
//	  maxTurns: 100
type Agent struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of Agent.
	// +required
	Spec AgentSpec `json:"spec,omitzero"`

	// status defines the observed state of Agent.
	// +optional
	Status AgentStatus `json:"status,omitzero"`
}

const (
	// AgentConditionReady indicates whether all referenced resources
	// (LLMProvider, Secrets) exist and are accessible.
	AgentConditionReady string = "Ready"
)

// AgentStatus defines the observed state of Agent. The operator
// validates that all referenced resources exist and reports readiness
// via standard Kubernetes conditions. An empty status (`status: {}`)
// is the initial state before the operator's first reconcile.
//
// +kubebuilder:validation:MinProperties=1
type AgentStatus struct {
	// conditions represent the latest available observations of the
	// Agent's state. The Ready condition summarizes whether all
	// referenced resources (LLMProvider, Secrets) are present.
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
