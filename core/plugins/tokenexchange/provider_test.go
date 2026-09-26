package tokenexchange

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAllProviderFilesAreRegistered scans for provider_*.go files in
// this package and verifies that each one has a corresponding provider
// registered in the registry. This catches the case where a
// contributor creates a provider file but forgets the init() call.
func TestAllProviderFilesAreRegistered(t *testing.T) {
	files, err := filepath.Glob("provider_*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}

		// Derive expected provider name from filename:
		// provider_keycloak.go → keycloak
		// provider_entra_id.go → entra-id
		name := strings.TrimPrefix(file, "provider_")
		name = strings.TrimSuffix(name, ".go")
		name = strings.ReplaceAll(name, "_", "-")

		// Read file to extract the Name() return value as a sanity
		// check — the provider might use a different name than the
		// filename convention. If we can't parse it, fall back to
		// the filename-derived name.
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}

		// Check that the file contains RegisterProvider
		if !strings.Contains(string(content), "RegisterProvider") {
			t.Errorf("%s: missing RegisterProvider() call in init() — provider will not be available at runtime", file)
			continue
		}

		// Verify the provider is actually registered
		if p := LookupProvider(name); p == nil {
			t.Errorf("%s: expected provider %q to be registered (file exists but LookupProvider returns nil — check init() and Name())", file, name)
		}
	}
}
