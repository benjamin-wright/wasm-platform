package controller

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

const pgMaxIdentifierLen = 63
const pgTruncPrefixLen = 47
const pgTruncHashLen = 15

// ValidatePGInputs returns an error if namespace or appName contains "--".
// Double-hyphen sequences would produce "____" after sanitisation, which
// collides with the "__" component separator used by PGDatabaseName and
// PGUsername. This cannot be caught by CRD validation because it applies to
// metadata.name / metadata.namespace, not spec fields.
func ValidatePGInputs(namespace, appName string) error {
	if strings.Contains(namespace, "--") {
		return fmt.Errorf("namespace %q contains consecutive hyphens ('--'), which is not supported for SQL-enabled applications", namespace)
	}
	if strings.Contains(appName, "--") {
		return fmt.Errorf("app name %q contains consecutive hyphens ('--'), which is not supported for SQL-enabled applications", appName)
	}
	return nil
}

// pgSanitise replaces hyphens with underscores, which is the only character
// substitution required to convert Kubernetes names into valid PG identifiers.
func pgSanitise(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

// pgTruncate shortens a PG identifier that exceeds 63 characters.
// It takes the first 47 characters of the full string, appends "_", then the
// first 15 hex characters of the lowercase SHA-256 of the full string.
func pgTruncate(full string) string {
	h := sha256.Sum256([]byte(full))
	return full[:pgTruncPrefixLen] + "_" + fmt.Sprintf("%x", h)[:pgTruncHashLen]
}

// pgIdentifier builds and, if necessary, truncates a PG identifier.
func pgIdentifier(full string) string {
	if len(full) > pgMaxIdentifierLen {
		return pgTruncate(full)
	}
	return full
}

// PGDatabaseName derives the PostgreSQL database name for a given application.
// Format: wasm_<namespace>__<app_name> with '-' replaced by '_'.
// The double underscore between namespace and app name prevents collisions between
// inputs like ("my-ns", "my-app") and ("my", "ns-my-app").
// If the result exceeds 63 characters it is truncated via pgTruncate.
func PGDatabaseName(namespace, appName string) string {
	full := "wasm_" + pgSanitise(namespace) + "__" + pgSanitise(appName)
	return pgIdentifier(full)
}

// PGUsername derives the PostgreSQL username for a given application user.
// Format: wasm_<namespace>__<app_name>__<user_name> with '-' replaced by '_'.
// Double underscores between each variable-length component prevent collisions.
// If the result exceeds 63 characters it is truncated via pgTruncate.
func PGUsername(namespace, appName, userName string) string {
	full := "wasm_" + pgSanitise(namespace) + "__" + pgSanitise(appName) + "__" + pgSanitise(userName)
	return pgIdentifier(full)
}
