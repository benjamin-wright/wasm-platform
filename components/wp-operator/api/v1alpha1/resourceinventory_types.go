package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ResourceCleanupPolicy string

const (
	CleanupWait            ResourceCleanupPolicy = "Wait"
	CleanupOrphanInventory ResourceCleanupPolicy = "OrphanInventory"
)

type ResourceDisposition string

const (
	ResourceActive  ResourceDisposition = "Active"
	ResourceRelease ResourceDisposition = "Release"
)

// InventoryApplicationReference identifies one lifetime of an Application.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="applicationRef is immutable"
type InventoryApplicationReference struct {
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:MinLength=1
	UID string `json:"uid"`
}

// ResourceBinding records an immutable resource identity and desired disposition.
// An empty UID is a persisted creation intent; a nonempty UID fences later reuse and deletion.
type ResourceBinding struct {
	// +optional
	// +kubebuilder:default=Active
	// +kubebuilder:validation:Enum=Active;Release
	Disposition ResourceDisposition `json:"disposition,omitempty"`
	// +kubebuilder:validation:Enum=PostgresDatabase;RedisDatabase;RedisCredential;PostgresCredential;PostgresMigrationSet
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// +optional
	UID         string `json:"uid,omitempty"`
	LogicalName string `json:"logicalName"`
	// Retained resources survive capability removal and Application deletion.
	// +optional
	Retained bool `json:"retained,omitempty"`
	// Credential identity fields contain no secrets.
	// +optional
	DatabaseRef string `json:"databaseRef,omitempty"`
	// +optional
	Database string `json:"database,omitempty"`
	// +optional
	Username string `json:"username,omitempty"`
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// ResourceInventorySpec is the authoritative record of binding and cleanup obligations.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.releasing) || !oldSelf.releasing || (has(self.releasing) && self.releasing)",message="releasing cannot be reversed"
type ResourceInventorySpec struct {
	ApplicationRef InventoryApplicationReference `json:"applicationRef"`
	// Releasing is monotonic: a retired inventory cannot provision resources again.
	// +optional
	Releasing bool `json:"releasing,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	Resources []ResourceBinding `json:"resources,omitempty"`
}

type ResourceInventoryStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	PendingReleases int32 `json:"pendingReleases,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ResourceInventory persists obligations independently of its originating Application.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ri
// +kubebuilder:printcolumn:name="Application",type=string,JSONPath=`.spec.applicationRef.name`
// +kubebuilder:printcolumn:name="Releasing",type=boolean,JSONPath=`.spec.releasing`
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=`.status.pendingReleases`
type ResourceInventory struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ResourceInventorySpec   `json:"spec"`
	Status            ResourceInventoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ResourceInventoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceInventory `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceInventory{}, &ResourceInventoryList{})
}
