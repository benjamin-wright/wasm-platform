package controller

import (
	"strings"
	"testing"
)

func TestValidatePGInputs(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		appName   string
		wantErr   bool
	}{
		{
			name:      "valid inputs — no error",
			namespace: "default",
			appName:   "my-app",
			wantErr:   false,
		},
		{
			name:      "double hyphen in namespace — error",
			namespace: "my--ns",
			appName:   "my-app",
			wantErr:   true,
		},
		{
			name:      "double hyphen in app name — error",
			namespace: "default",
			appName:   "my--app",
			wantErr:   true,
		},
		{
			name:      "double hyphen in both — error",
			namespace: "my--ns",
			appName:   "my--app",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePGInputs(tt.namespace, tt.appName)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePGInputs(%q, %q) error = %v, wantErr %v", tt.namespace, tt.appName, err, tt.wantErr)
			}
		})
	}
}

func TestPGDatabaseName(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		appName   string
		want      string
	}{
		{
			name:      "simple names — no truncation",
			namespace: "default",
			appName:   "my-app",
			want:      "wasm_default__my_app",
		},
		{
			name:      "hyphens replaced in both parts",
			namespace: "my-ns",
			appName:   "some-app",
			want:      "wasm_my_ns__some_app",
		},
		{
			name:      "exactly 63 characters — no truncation",
			namespace: "default",
			// "wasm_default__" = 14 chars; pad appName to 49 chars → total 63
			appName: strings.Repeat("a", 49),
			want:    "wasm_default__" + strings.Repeat("a", 49),
		},
		{
			name:      "64 characters — truncated",
			namespace: "default",
			// "wasm_default__" = 14 chars; 50-char app name → 64 chars total
			appName: strings.Repeat("a", 50),
			want:    "wasm_default__aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_3e11ffe7715279b",
		},
		{
			name:      "very long names — truncated",
			namespace: "very-long-namespace-name",
			appName:   "very-long-application-name-that-exceeds-limits",
			want:      "wasm_very_long_namespace_name__very_long_applic_99fe0940123e9be",
		},
		{
			name:      "collision guard — ns boundary distinct from app boundary",
			namespace: "my-ns",
			appName:   "my-app",
			// must differ from namespace="my" appName="ns-my-app"
			want: "wasm_my_ns__my_app",
		},
		{
			name:      "collision guard — other side",
			namespace: "my",
			appName:   "ns-my-app",
			want:      "wasm_my__ns_my_app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PGDatabaseName(tt.namespace, tt.appName)
			if got != tt.want {
				t.Errorf("PGDatabaseName(%q, %q) = %q, want %q", tt.namespace, tt.appName, got, tt.want)
			}
			if len(got) > pgMaxIdentifierLen {
				t.Errorf("PGDatabaseName(%q, %q) len=%d exceeds %d", tt.namespace, tt.appName, len(got), pgMaxIdentifierLen)
			}
		})
	}
}

func TestPGUsername(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		appName   string
		userName  string
		want      string
	}{
		{
			name:      "simple names — no truncation",
			namespace: "default",
			appName:   "my-app",
			userName:  "app",
			want:      "wasm_default__my_app__app",
		},
		{
			name:      "hyphens replaced in all parts",
			namespace: "my-ns",
			appName:   "some-app",
			userName:  "read-user",
			want:      "wasm_my_ns__some_app__read_user",
		},
		{
			name:      "exactly 63 characters — no truncation",
			namespace: "default",
			// "wasm_default__a__" = 17 chars; pad userName to 46 chars → total 63
			appName:  "a",
			userName: strings.Repeat("b", 46),
			want:     "wasm_default__a__" + strings.Repeat("b", 46),
		},
		{
			name:      "64 characters — truncated",
			namespace: "default",
			// "wasm_default__a__" = 17 chars; 47-char userName → 64 chars total
			appName:  "a",
			userName: strings.Repeat("b", 47),
			want:     "wasm_default__a__bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb_8fb045f9814e76e",
		},
		{
			name:      "very long names — truncated",
			namespace: "very-long-namespace-name",
			appName:   "very-long-application-name",
			userName:  "read-only-replica-user",
			want:      "wasm_very_long_namespace_name__very_long_applic_81ddd279d3af211",
		},
		{
			name:      "implicit app user",
			namespace: "default",
			appName:   "hello-world",
			userName:  "app",
			want:      "wasm_default__hello_world__app",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PGUsername(tt.namespace, tt.appName, tt.userName)
			if got != tt.want {
				t.Errorf("PGUsername(%q, %q, %q) = %q, want %q", tt.namespace, tt.appName, tt.userName, got, tt.want)
			}
			if len(got) > pgMaxIdentifierLen {
				t.Errorf("PGUsername(%q, %q, %q) len=%d exceeds %d", tt.namespace, tt.appName, tt.userName, len(got), pgMaxIdentifierLen)
			}
		})
	}
}
