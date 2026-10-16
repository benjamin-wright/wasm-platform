package controller

import "github.com/benjamin-wright/wasm-platform/wp-operator/internal/provisioning"

const pgMaxIdentifierLen = provisioning.PGMaxIdentifierLen

func ValidatePGInputs(namespace, appName string) error {
	return provisioning.ValidatePGInputs(namespace, appName)
}
func PGDatabaseName(namespace, appName string) string {
	return provisioning.PGDatabaseName(namespace, appName)
}
func PGUsername(namespace, appName, userName string) string {
	return provisioning.PGUsername(namespace, appName, userName)
}

// K8sCredentialName identifies a legacy credential for a verified transition.
func K8sCredentialName(namespace, appName, userName string) string {
	return provisioning.K8sCredentialName(namespace, appName, userName)
}

// MigrationSetName identifies a legacy migration for a verified transition.
func MigrationSetName(namespace, appName string) string {
	return provisioning.MigrationSetName(namespace, appName)
}
