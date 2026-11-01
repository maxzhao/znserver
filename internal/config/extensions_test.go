package config

import (
	"path/filepath"
	"testing"
)

func TestExtensionsGlobalRootEnvironment(t *testing.T) {
	t.Setenv("ZENNOTES_CONFIG_PATH", filepath.Join(t.TempDir(), "server.json"))
	t.Setenv("ZENNOTES_AUTH_TOKEN", "")
	t.Setenv("ZENNOTES_AUTH_TOKEN_FILE", "")
	for _, value := range []string{"", "/extensions"} {
		t.Setenv("ZENNOTES_EXTENSIONS_GLOBAL_ROOT", value)
		if got := Load().ExtensionsGlobalRoot; got != value {
			t.Fatalf("global root %q, want %q", got, value)
		}
	}
}
