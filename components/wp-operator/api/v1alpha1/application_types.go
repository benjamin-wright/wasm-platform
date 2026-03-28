package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ApplicationSpec defines the desired state of an Application.
type ApplicationSpec struct {
	// Module is the OCI URI for the .wasm module.
	// Use a digest-pinned reference (@sha256:…) for deterministic deployments.
	// Format: oci://<registry>/<repository>@sha256:<digest>.
	// +kubebuilder:validation:Required
	Module string `json:"module"`

	// Topic is the message subject the execution host subscribes to.
	// Messages on this subject invoke the module's on-message export.
	// +kubebuilder:validation:Required
	Topic string `json:"topic"`

	// Env is an optional map of environment variables injected into the module's runtime configuration.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// SQL is the logical database name exposed to the module via the sql host import.
	// Must correspond to a provisioned database managed by the db-operator.
	// Omit to disable SQL access.
	// +optional
	SQL string `json:"sql,omitempty"`

	// KeyValue is the key prefix for the module's key-value namespace.
	// Keys written by the module are namespaced by <namespace>/<prefix>/ to prevent conflicts.
	// No external provisioning required — isolation is enforced by the execution host at runtime.
	// Omit to disable KV access.
	// +optional
	KeyValue string `json:"keyValue,omitempty"`
}

// ApplicationStatus defines the observed state of an Application.
type ApplicationStatus struct {
	// ResolvedImage is the fully qualified OCI reference with resolved digest.
	// +optional
	ResolvedImage string `json:"resolvedImage,omitempty"`

	// Conditions is the standard Kubernetes condition list.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Application is the primary resource for wasm-platform.
// Each instance declares a single deployable WASM module and its runtime requirements.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Module",type=string,JSONPath=`.spec.module`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ApplicationSpec   `json:"spec,omitempty"`
	Status ApplicationStatus `json:"status,omitempty"`
}

// ApplicationList contains a list of Application resources.
// +kubebuilder:object:root=true
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Application `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Application{}, &ApplicationList{})
}
