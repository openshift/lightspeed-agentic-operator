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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition type for AgenticOLSConfig status.
const (
	// AgenticOLSConfigConditionSuspended tracks whether the system kill
	// switch is active. True means all proposals have been emergency-stopped
	// and the system is suspended; False means the admin has deactivated
	// suspension and the system is accepting new proposals.
	AgenticOLSConfigConditionSuspended = "Suspended"
)

// AgenticOLSConfigSpec defines the desired state of AgenticOLSConfig.
//
// +kubebuilder:validation:MinProperties=1
type AgenticOLSConfigSpec struct {
	// suspended halts all agentic operations cluster-wide when set to true.
	// All non-terminal proposals are immediately terminated with an
	// EmergencyStopped condition. Setting back to false re-enables the
	// system for new proposals only — EmergencyStopped proposals remain
	// terminal and must be recreated explicitly.
	// +optional
	// +default=false
	Suspended bool `json:"suspended,omitempty"` //nolint:kubeapilinter // kill switch is genuinely binary; bool is the right type
}

// AgenticOLSConfigStatus defines the observed state of AgenticOLSConfig.
//
// +kubebuilder:validation:MinProperties=1
type AgenticOLSConfigStatus struct {
	// conditions represent the latest available observations of the
	// config's state. The "Suspended" condition tracks whether the
	// kill switch is active and all proposals have been terminated.
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspended`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="AgenticOLSConfig must be named 'cluster' (singleton)"

// AgenticOLSConfig is a cluster-scoped singleton that controls system-wide
// agentic behavior. The cluster admin creates a single AgenticOLSConfig
// named "cluster". When spec.suspended is true, all non-terminal proposals
// are terminated and no new workflow steps are started.
//
// When no AgenticOLSConfig CR exists, the system behaves as if
// suspended is false — the CR is not required for normal operation.
//
// Example:
//
//	apiVersion: agentic.openshift.io/v1alpha1
//	kind: AgenticOLSConfig
//	metadata:
//	  name: cluster
//	spec:
//	  suspended: false
type AgenticOLSConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired system configuration.
	// +required
	Spec AgenticOLSConfigSpec `json:"spec,omitzero"`

	// status defines the observed state of AgenticOLSConfig.
	// +optional
	Status AgenticOLSConfigStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgenticOLSConfigList contains a list of AgenticOLSConfig.
type AgenticOLSConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgenticOLSConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgenticOLSConfig{}, &AgenticOLSConfigList{})
}
