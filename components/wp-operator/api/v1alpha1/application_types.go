package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HttpMethod is a valid HTTP method string.
// +kubebuilder:validation:Enum=GET;HEAD;POST;PUT;DELETE;PATCH;OPTIONS
type HttpMethod string

// HttpTrigger defines the HTTP trigger configuration for a function.
type HttpTrigger struct {
	// Path is the URL path the gateway exposes for this function.
	// Must start with '/'. Must be unique cluster-wide.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`

	// Methods is the list of HTTP methods the gateway accepts on this path.
	// If omitted, all methods are allowed.
	// Valid values: GET, HEAD, POST, PUT, DELETE, PATCH, OPTIONS.
	// +optional
	Methods []HttpMethod `json:"methods,omitempty"`
}

// MethodStrings returns Methods as a plain []string for use with proto and store types.
func (h *HttpTrigger) MethodStrings() []string {
	out := make([]string, len(h.Methods))
	for i, m := range h.Methods {
		out[i] = string(m)
	}
	return out
}

// FunctionTrigger defines the trigger for a function.
// Exactly one of HTTP or Topic must be set.
// +kubebuilder:validation:XValidation:rule="has(self.http) != has(self.topic)",message="exactly one of trigger.http or trigger.topic must be set"
type FunctionTrigger struct {
	// HTTP declares this function as HTTP-triggered and exposed via the gateway.
	// The gateway auto-generates the internal NATS subject as http.<namespace>.<app-name>.<function-name>.
	// Mutually exclusive with Topic.
	// +optional
	HTTP *HttpTrigger `json:"http,omitempty"`

	// Topic is the message subject the execution host subscribes to.
	// Messages on this subject invoke the function's on-message export.
	// Must not contain wildcard characters ('*' or '>'); topics are unique cluster-wide.
	// Mutually exclusive with HTTP.
	// +optional
	// +kubebuilder:validation:Pattern=`^[^*>]+$`
	Topic string `json:"topic,omitempty"`
}

// FunctionSpec declares a single deployable function within an Application.
type FunctionSpec struct {
	// Name is the identifier for this function, unique within the Application.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Module is the OCI URI for the .wasm module.
	// Use a digest-pinned reference (@sha256:…) for deterministic deployments.
	// Format: oci://<registry>/<repository>@sha256:<digest>.
	// +kubebuilder:validation:Required
	Module string `json:"module"`

	// Trigger defines the event source for this function.
	// +kubebuilder:validation:Required
	Trigger FunctionTrigger `json:"trigger"`
}

// ApplicationSpec defines the desired state of an Application.
type ApplicationSpec struct {
	// Functions is the list of deployable functions in this Application.
	// Each function has its own module and trigger.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Functions []FunctionSpec `json:"functions"`

	// Env is an optional map of environment variables injected into all functions'
	// runtime configuration.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// SQL is the logical database name exposed to all functions via the sql host import.
	// Must correspond to a provisioned database managed by the db-operator.
	// Omit to disable SQL access.
	// +optional
	SQL string `json:"sql,omitempty"`

	// KeyValue is the key prefix for all functions' key-value namespace.
	// Keys written by functions are namespaced by <namespace>/<prefix>/ to prevent conflicts.
	// No external provisioning required — isolation is enforced by the execution host at runtime.
	// Omit to disable KV access.
	// +optional
	KeyValue string `json:"keyValue,omitempty"`
}

// ApplicationStatus defines the observed state of an Application.
type ApplicationStatus struct {
	// Conditions is the standard Kubernetes condition list.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Application is the primary resource for wasm-platform.
// Each instance declares one or more deployable WASM functions and their shared runtime requirements.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
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
