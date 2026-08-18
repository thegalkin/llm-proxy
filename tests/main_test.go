package proxy_test

import (
	"os"
	"path/filepath"
	"testing"

	"llm-proxy/internal/proxy"
)

// --- helpers ---

func loadTestConfig(t *testing.T) *proxy.Config {
	t.Helper()
	c, err := proxy.LoadRoutingConfigFromString(testConfigYAML)
	if err != nil {
		t.Fatalf("loadRoutingConfig: %v", err)
	}
	return &c
}

func TestRoutingPriority_OpencodeGo(t *testing.T) {
	c := loadTestConfig(t)
	cases := []struct {
		name string
		key  string
	}{
		{"opencode-go/kimi-k3", "opencode-go/kimi-k3"},
		{"opencode-go/glm-5.2", "opencode-go/glm-5.2"},
		{"opencode-go/deepseek-v4-flash", "opencode-go/deepseek-v4-flash"},
		{"opencode-zen/kimi-k3", "opencode-zen/kimi-k3"},
		{"opencode-zen/glm-5.1", "opencode-zen/glm-5.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			us := c.ResolveRule(tc.key)
			if us.Type != "opencode-go" {
				t.Errorf("type = %q, want opencode-go (ruleName=%s)", us.Type, us.BaseURL)
			}
			if us.Model != "deepseek-v4-flash" {
				t.Errorf("model = %q, want deepseek-v4-flash", us.Model)
			}
			if us.ReasoningEffort != "max" {
				t.Errorf("reasoning_effort = %q, want max", us.ReasoningEffort)
			}
		})
	}
}

func TestRoutingPriority_CatchAll(t *testing.T) {
	c := loadTestConfig(t)
	cases := []string{
		"random/anything",
		"anthropic/claude-3",
		"openai/gpt-4",
	}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			us := c.ResolveRule(key)
			wantType := "opencode-go"
			wantModel := "deepseek-v4-flash"
			wantEff := "max"
			if us.Type != wantType {
				t.Errorf("catch-all: type = %q, want %q", us.Type, wantType)
			}
			if us.Model != wantModel {
				t.Errorf("catch-all: model = %q, want %q", us.Model, wantModel)
			}
			if us.ReasoningEffort != wantEff {
				t.Errorf("catch-all: reasoning_effort = %q, want %q", us.ReasoningEffort, wantEff)
			}
		})
	}
}

func TestRoutingPriority_Order(t *testing.T) {
	c := loadTestConfig(t)
	if c.Rules[0].MatchExpr != "opencode-go/*" {
		t.Errorf("rule[0] = %q, want opencode-go/*", c.Rules[0].MatchExpr)
	}
	if c.Rules[len(c.Rules)-1].MatchExpr != "*" {
		t.Errorf("catch-all rule = %q, want *", c.Rules[len(c.Rules)-1].MatchExpr)
	}
}

func TestApplyRule_ReasoningEffort(t *testing.T) {
	c := loadTestConfig(t)
	us := c.ResolveRule("opencode-go/kimi-k3")
	if us.ReasoningEffort != "max" {
		t.Fatalf("setup: reasoning_effort = %q", us.ReasoningEffort)
	}
	body := []byte(`{"model":"opencode-go/kimi-k3","messages":[],"max_tokens":4}`)
	out := proxy.ApplyReasoningEffort(body, us.ReasoningEffort)
	if !contains(out, []byte(`"reasoning_effort":"max"`)) {
		t.Errorf("expected reasoning_effort=max in body, got %s", out)
	}
}

func TestApplyRule_ReasoningEffortNoOp(t *testing.T) {
	body := []byte(`{"model":"opencode-go/kimi-k3","messages":[]}`)
	out := proxy.ApplyReasoningEffort(body, "")
	if contains(out, []byte("reasoning_effort")) {
		t.Errorf("no reasoning_effort should be injected, got %s", out)
	}
}

func TestDecideKeyNoDuplicatePrefix(t *testing.T) {
	c := loadTestConfig(t)
	cases := []struct {
		name         string
		body         string
		hostProvider string
		wantType     string
		wantModel    string
	}{
		{"client-prefixed kimi", `{"model":"opencode-go/kimi-k3","messages":[]}`, "opencode-go", "opencode-go", "deepseek-v4-flash"},
		{"bare model gets prefix", `{"model":"kimi-k3","messages":[]}`, "opencode-go", "opencode-go", "deepseek-v4-flash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rd := proxy.Decide(c, []byte(tc.body), "/v1/chat/completions", tc.hostProvider)
			if rd.Upstream.Type != tc.wantType {
				t.Errorf("type = %q, want %q (ruleName=%s)", rd.Upstream.Type, tc.wantType, rd.RuleName)
			}
			if rd.Upstream.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", rd.Upstream.Model, tc.wantModel)
			}
		})
	}
}

func TestJoinTarget(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		pattern string
		want    string
	}{
		{"dup-v1", "https://opencode.ai/zen/go/v1", "/v1/chat/completions", "https://opencode.ai/zen/go/v1/chat/completions"},
		{"clean-pattern", "https://opencode.ai/zen/go/v1", "/chat/completions", "https://opencode.ai/zen/go/v1/chat/completions"},
		{"trailing-slash-base", "https://opencode.ai/zen/go/v1/", "/chat/completions", "https://opencode.ai/zen/go/v1/chat/completions"},
		{"no-pattern", "https://opencode.ai/zen/go/v1", "", "https://opencode.ai/zen/go/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxy.JoinTarget(tc.base, tc.pattern); got != tc.want {
				t.Errorf("joinTarget(%q, %q) = %q, want %q", tc.base, tc.pattern, got, tc.want)
			}
		})
	}
}

func TestRoutingTargetURL(t *testing.T) {
	c := loadTestConfig(t)
	opencodeGoURL := "https://opencode.ai/zen/go/v1/chat/completions"
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"go-kimi", "opencode-go/kimi-k3", opencodeGoURL},
		{"zen-glm", "opencode-zen/glm-5.1", opencodeGoURL},
		{"catch-all", "random/anything", opencodeGoURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			us := c.ResolveRule(tc.key)
			got := proxy.JoinTarget(us.BaseURL, us.URLPattern)
			if got != tc.want {
				t.Errorf("rule %q -> target %q, want %q (type=%s)", tc.key, got, tc.want, us.Type)
			}
		})
	}
}

// --- fixture ---

const testConfigYAML = `
listen_addr: 127.0.0.1:8443
default_to: opencode-go/deepseek-v4-flash
default_base_url: https://opencode.ai/zen/go/v1
default_url_pattern: /v1/chat/completions
default_reasoning_effort: max

rules:
  - priority: 1
    from: opencode-go/*
    to: opencode-go/deepseek-v4-flash
    reasoning_effort: max

  - priority: 1
    from: opencode-zen/*
    to: opencode-go/deepseek-v4-flash
    reasoning_effort: max

  - priority: 99
    from: "*"
    to: opencode-go/deepseek-v4-flash
    reasoning_effort: max
`

// --- contains helper ---

func contains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// --- env-var override: LLM_PROXY_CONFIG must beat the default path ---
// Runs against a temp HOME so the test never touches the real
// ~/.config/llm-proxy (the old version wrote to and deleted the real
// directory, breaking the systemd unit's ReadWritePaths mount).
func TestConfigPathOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "") // UserConfigDir falls back to $HOME/.config
	custom := t.TempDir() + "/custom-config.yaml"
	const wantListen = "127.0.0.1:19999"
	yaml := "listen_addr: " + wantListen + "\ndefault_to: opencode-go/deepseek-v4-flash\n"
	if err := os.WriteFile(custom, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	t.Setenv("LLM_PROXY_CONFIG", custom)
	stale := filepath.Join(home, ".config", "llm-proxy", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(stale, []byte("listen_addr: 1.2.3.4:9999\n"), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	cfg := proxy.LoadRoutingConfig()
	if cfg.ListenAddr != wantListen {
		t.Fatalf("listen_addr = %q want %q - env var LLM_PROXY_CONFIG was not honored", cfg.ListenAddr, wantListen)
	}
}
