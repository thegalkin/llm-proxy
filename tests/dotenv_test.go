package proxy_test

import (
	"os"
	"path/filepath"
	"testing"

	"llm-proxy/internal/proxy"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "FOO_BAR=hello\nQUOTED=\"world\"\nEXISTING=no\n# comment\n\nNO_EQ\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env fixture: %v", err)
	}

	restoreEnv := func(key, val string, had bool) {
		t.Cleanup(func() {
			if had {
				os.Setenv(key, val)
			} else {
				os.Unsetenv(key)
			}
		})
	}
	for _, key := range []string{"FOO_BAR", "QUOTED"} {
		v, had := os.LookupEnv(key)
		os.Unsetenv(key)
		restoreEnv(key, v, had)
	}
	t.Setenv("EXISTING", "yes")
	t.Setenv("ENV_FILE", path)

	proxy.LoadDotEnv()

	if got := os.Getenv("FOO_BAR"); got != "hello" {
		t.Errorf("FOO_BAR = %q, want hello", got)
	}
	if got := os.Getenv("QUOTED"); got != "world" {
		t.Errorf("QUOTED = %q, want world (quotes stripped)", got)
	}
	if got := os.Getenv("EXISTING"); got != "yes" {
		t.Errorf("EXISTING = %q, want yes (existing vars must not be overwritten)", got)
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "nope.env"))
	proxy.LoadDotEnv() // must not panic
}
