package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// LoadDotEnv reads KEY=VALUE pairs from a file and seeds the process
// environment with any variable that is not already set. Missing file,
// blank lines, comments, and quote-stripping must all be handled.
func TestLoadDotEnv(t *testing.T) {
	t.Run("missing file is a no-op", func(t *testing.T) {
		t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
		LoadDotEnv() // must not panic
	})
	t.Run("missing ENV_FILE defaults to .env in cwd", func(t *testing.T) {
		// Verify the empty-ENV_FILE branch by pointing cwd at an empty
		// TempDir so the fallback "./.env" path doesn't exist.
		t.Setenv("ENV_FILE", "")
		dir := t.TempDir()
		cwd, _ := os.Getwd()
		os.Chdir(dir)
		t.Cleanup(func() { os.Chdir(cwd) })
		LoadDotEnv()
	})
	t.Run("present file with KEY=VALUE sets env", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("HELLO=world\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Setenv("ENV_FILE", path)
		os.Unsetenv("HELLO")
		t.Cleanup(func() { os.Unsetenv("HELLO") })
		LoadDotEnv()
		if got := os.Getenv("HELLO"); got != "world" {
			t.Errorf("HELLO = %q, want world", got)
		}
	})
	t.Run("blank lines and comments ignored", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("\n# a comment\nNOEQ\nANOTHER=ok\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Setenv("ENV_FILE", path)
		os.Unsetenv("ANOTHER")
		t.Cleanup(func() { os.Unsetenv("ANOTHER") })
		LoadDotEnv()
		if got := os.Getenv("ANOTHER"); got != "ok" {
			t.Errorf("ANOTHER = %q, want ok", got)
		}
	})
	t.Run("KEY without equals sign is ignored", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("BAREKEY\nFOUND=found\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Setenv("ENV_FILE", path)
		os.Unsetenv("BAREKEY")
		os.Unsetenv("FOUND")
		t.Cleanup(func() {
			os.Unsetenv("BAREKEY")
			os.Unsetenv("FOUND")
		})
		LoadDotEnv()
		if got := os.Getenv("BAREKEY"); got != "" {
			t.Errorf("BAREKEY = %q, want empty (lines without '=' are skipped)", got)
		}
		if got := os.Getenv("FOUND"); got != "found" {
			t.Errorf("FOUND = %q, want found", got)
		}
	})
	t.Run("quotes stripped from value", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("DQ=\"double\"\nSQ='single'\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Setenv("ENV_FILE", path)
		os.Unsetenv("DQ")
		os.Unsetenv("SQ")
		t.Cleanup(func() {
			os.Unsetenv("DQ")
			os.Unsetenv("SQ")
		})
		LoadDotEnv()
		if got := os.Getenv("DQ"); got != "double" {
			t.Errorf("DQ = %q, want double", got)
		}
		if got := os.Getenv("SQ"); got != "single" {
			t.Errorf("SQ = %q, want single", got)
		}
	})
	t.Run("dollar sign kept literal — no expansion", func(t *testing.T) {
		// LoadDotEnv does NOT run os.ExpandEnv; ${OTHER} is written
		// verbatim. Pin that contract.
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("REF=${OTHER}\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Setenv("ENV_FILE", path)
		t.Setenv("OTHER", "expanded")
		os.Unsetenv("REF")
		t.Cleanup(func() { os.Unsetenv("REF") })
		LoadDotEnv()
		if got := os.Getenv("REF"); got != "${OTHER}" {
			t.Errorf("REF = %q, want literal ${OTHER}", got)
		}
	})
	t.Run("existing env wins — .env does not overwrite", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ".env")
		if err := os.WriteFile(path, []byte("LOCKED=from-dotenv\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Setenv("ENV_FILE", path)
		t.Setenv("LOCKED", "from-env")
		LoadDotEnv()
		if got := os.Getenv("LOCKED"); got != "from-env" {
			t.Errorf("LOCKED = %q, want from-env (existing env must not be overwritten)", got)
		}
	})
}
