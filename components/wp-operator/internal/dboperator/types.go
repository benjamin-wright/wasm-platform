// Package dboperator provides minimal Go types for the db-operator CRDs used
// by the wp-operator. Only the fields required by the reconciler are modelled
// here; the full CRD schema lives in the external db-operator repository.
package dboperator

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SchemeGroupVersion is the GroupVersion for all db-operator CRDs.
var SchemeGroupVersion = schema.GroupVersion{
	Group:   "db-operator.benjamin-wright.github.com",
	Version: "v1alpha1",
}

// SchemeBuilder registers the types defined in this package with a runtime.Scheme.
var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion,
		&PostgresCredential{},
		&PostgresCredentialList{},
	)
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

// PostgresCredentialSpec mirrors the subset of the db-operator
// PostgresCredential spec required by the wp-operator reconciler.
type PostgresCredentialSpec struct {
	// DatabaseRef is the name of the PostgresDatabase CR this credential targets.
	DatabaseRef string `json:"databaseRef"`
	// Username is the PostgreSQL role name that will be created.
	Username string `json:"username"`
	// SecretName is the name of the Secret the db-operator will populate with
	// PGUSER, PGPASSWORD, PGHOST, PGPORT, and PGDATABASE.
	SecretName string `json:"secretName"`
	// Permissions is the list of per-database privilege grants.
	Permissions []PostgresPermissionEntry `json:"permissions,omitempty"`
}

// PostgresPermissionEntry grants a set of privileges on a list of databases.
type PostgresPermissionEntry struct {
	Databases   []string `json:"databases"`
	Permissions []string `json:"permissions"`
}

// PostgresCredential instructs the db-operator to provision a PostgreSQL role
// and credentials Secret.
type PostgresCredential struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PostgresCredentialSpec `json:"spec,omitempty"`
}

// PostgresCredentialList is the list variant of PostgresCredential.
type PostgresCredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresCredential `json:"items"`
}

// DeepCopyObject implements runtime.Object for PostgresCredential.
func (in *PostgresCredential) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(PostgresCredential)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies all fields of in into out.
func (in *PostgresCredential) DeepCopyInto(out *PostgresCredential) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.DatabaseRef = in.Spec.DatabaseRef
	out.Spec.Username = in.Spec.Username
	out.Spec.SecretName = in.Spec.SecretName
	if in.Spec.Permissions != nil {
		out.Spec.Permissions = make([]PostgresPermissionEntry, len(in.Spec.Permissions))
		for i, p := range in.Spec.Permissions {
			out.Spec.Permissions[i] = PostgresPermissionEntry{
				Databases:   append([]string{}, p.Databases...),
				Permissions: append([]string{}, p.Permissions...),
			}
		}
	}
}

// DeepCopyObject implements runtime.Object for PostgresCredentialList.
func (in *PostgresCredentialList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(PostgresCredentialList)
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]PostgresCredential, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}
