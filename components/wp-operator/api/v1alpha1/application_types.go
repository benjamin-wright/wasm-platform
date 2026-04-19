package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HttpMethod is a valid HTTP method string.
// +kubebuilder:validation:Enum=GET;HEAD;POST;PUT;DELETE;PATCH;OPTIONS
type HttpMethod string

// MetricType is the Prometheus metric type for a user-defined metric.
// +kubebuilder:validation:Enum=counter;gauge
type MetricType string

const (
	MetricTypeCounter MetricType = "counter"
	MetricTypeGauge   MetricType = "gauge"
)

// MetricDefinition declares a single user-defined Prometheus metric.
// Names are globally unique — the operator enforces cluster-wide uniqueness at reconcile time.
//
// +kubebuilder:validation:XValidation:rule="!self.name.startsWith('__')",message="metric name must not start with '__' (Prometheus reserved prefix)"
// +kubebuilder:validation:XValidation:rule="!has(self.labels) || !self.labels.exists(l, l == 'app_name' || l == 'app_namespace')",message="labels must not include 'app_name' or 'app_namespace' (host-injected labels)"
type MetricDefinition struct {
	// Name is the Prometheus metric name. Must match [a-zA-Z_:][a-zA-Z0-9_:]*, max 64 characters.
	// Must not start with '__' (Prometheus reserved prefix). Unique cluster-wide.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_:][a-zA-Z0-9_:]{0,63}$`
	Name string `json:"name"`

	// Type is the Prometheus metric type.
	// +kubebuilder:validation:Required
	Type MetricType `json:"type"`

	// Labels is the list of label key names for this metric.
	// Each key must match [a-zA-Z_][a-zA-Z0-9_]*. Max 10 entries.
	// Must not include 'app_name' or 'app_namespace' (injected by the execution host).
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Labels []string `json:"labels,omitempty"`
}

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

// SQLGrant is a PostgreSQL table-level privilege.
// +kubebuilder:validation:Enum=SELECT;INSERT;UPDATE;DELETE;TRUNCATE;REFERENCES;TRIGGER;ALL
type SQLGrant string

const (
	SQLGrantSelect     SQLGrant = "SELECT"
	SQLGrantInsert     SQLGrant = "INSERT"
	SQLGrantUpdate     SQLGrant = "UPDATE"
	SQLGrantDelete     SQLGrant = "DELETE"
	SQLGrantTruncate   SQLGrant = "TRUNCATE"
	SQLGrantReferences SQLGrant = "REFERENCES"
	SQLGrantTrigger    SQLGrant = "TRIGGER"
	SQLGrantAll        SQLGrant = "ALL"
)

// SQLTablePermission maps a set of PostgreSQL privileges to a list of tables.
type SQLTablePermission struct {
	// Tables is the list of table names to which the grants apply.
	// If absent, grants apply to all tables.
	// +optional
	Tables []string `json:"tables,omitempty"`

	// Grant is the list of PostgreSQL privileges to grant on the specified tables.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Grant []SQLGrant `json:"grant"`
}

// SQLUserSpec declares a named database user and its table-level permissions.
//
// +kubebuilder:validation:XValidation:rule="self.name != 'migrations'",message="'migrations' is a reserved SQL user name"
type SQLUserSpec struct {
	// Name is the logical user identifier. Used to derive the PG username and referenced
	// by function sqlUser fields. The name 'migrations' is reserved.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Permissions is the list of table-level grants for this user.
	// If absent, ALL is granted on all tables.
	// +optional
	Permissions []SQLTablePermission `json:"permissions,omitempty"`
}

// SQLSpec configures SQL database access for an Application.
// An empty struct (sql: {}) enables SQL with a single implicit 'app' user granted ALL
// on all tables. Functions are implicitly bound to the 'app' user when users is absent.
type SQLSpec struct {
	// Users is the list of named database users to provision.
	// If absent or empty, a single user named 'app' is provisioned with ALL on all tables
	// and all functions are implicitly bound to that user.
	// If non-empty, only the listed users are provisioned; functions must opt in via sqlUser.
	// +optional
	Users []SQLUserSpec `json:"users,omitempty"`
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

	// SQLUser is the name of the SQL user (from spec.sql.users) this function uses.
	// When spec.sql is set and spec.sql.users is absent or empty, functions implicitly use
	// the 'app' user and this field is ignored.
	// When spec.sql.users is non-empty, a function with this field set is granted access
	// under the named user; a function without this field has no SQL access.
	// When spec.sql is absent, this field has no effect.
	// +optional
	SQLUser *string `json:"sqlUser,omitempty"`
}

// ApplicationSpec defines the desired state of an Application.
//
// +kubebuilder:validation:XValidation:rule="!has(self.sql) || !has(self.sql.users) || self.sql.users.size() == 0 || self.functions.all(f, !has(f.sqlUser) || self.sql.users.exists(u, u.name == f.sqlUser))",message="each function's sqlUser must reference a user name defined in spec.sql.users"
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

	// SQL configures optional SQL database access for this Application.
	// When absent, no SQL access is provisioned.
	// When present as an empty struct (sql: {}), a single 'app' user is provisioned
	// with ALL on all tables, and all functions are implicitly bound to it.
	// When spec.sql.users is non-empty, only the listed users are provisioned.
	// +optional
	SQL *SQLSpec `json:"sql,omitempty"`

	// Metrics is the list of user-defined Prometheus metrics declared by this Application.
	// Names must be unique within the Application and cluster-wide; the operator enforces
	// cluster-wide uniqueness at reconcile time (oldest Application wins).
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Metrics []MetricDefinition `json:"metrics,omitempty"`
}

// ApplicationStatus defines the observed state of an Application.
type ApplicationStatus struct {
	// Conditions is the standard Kubernetes condition list.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// SQLDatabaseName is the derived PostgreSQL database name for this Application.
	// Populated only when spec.sql is set.
	// +optional
	SQLDatabaseName string `json:"sqlDatabaseName,omitempty"`

	// SQLUsernames is the list of derived PostgreSQL usernames provisioned for this
	// Application, one per entry in spec.sql.users (or a single 'app' user when
	// spec.sql.users is absent). Populated only when spec.sql is set.
	// +optional
	SQLUsernames []string `json:"sqlUsernames,omitempty"`
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
