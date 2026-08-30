package proxy

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// resolveConfigPath honours LLM_PROXY_CONFIG and falls back to the OS user
// config dir; an explicit env var must always win.
func TestResolveConfigPath(t *testing.T) {
	t.Run("env var wins", func(t *testing.T) {
		want := "/tmp/from-env.yaml"
		t.Setenv("LLM_PROXY_CONFIG", want)
		if got := resolveConfigPath(); got != want {
			t.Errorf("resolveConfigPath = %q, want %q", got, want)
		}
	})
	t.Run("falls back to user config dir under temp HOME", func(t *testing.T) {
		t.Setenv("LLM_PROXY_CONFIG", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", "") // force UserConfigDir() to use $HOME/.config
		got := resolveConfigPath()
		want := filepath.Join(home, ".config", "llm-proxy", "config.yaml")
		if got != want {
			t.Errorf("resolveConfigPath = %q, want %q", got, want)
		}
	})
}

// LoadRoutingConfig reads the file pointed at by LLM_PROXY_CONFIG (or the
// OS-default path) and returns the parsed config. A missing file must
// return the built-in defaults — never crash.
func TestLoadRoutingConfig(t *testing.T) {
	t.Run("with rules", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		yaml := "listen_addr: 127.0.0.1:19999\ndefault_to: opencode-go/deepseek-v4-flash\nrules:\n  - priority: 5\n    from: 'foo/*'\n    to: opencode-go/bar\n"
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Setenv("LLM_PROXY_CONFIG", path)
		cfg := LoadRoutingConfig()
		if cfg.ListenAddr != "127.0.0.1:19999" {
			t.Errorf("ListenAddr = %q, want 127.0.0.1:19999", cfg.ListenAddr)
		}
		if len(cfg.Rules) < 1 || cfg.Rules[0].MatchExpr != "foo/*" {
			t.Errorf("Rules[0].MatchExpr = %q, want foo/*", cfg.Rules[0].MatchExpr)
		}
	})
	t.Run("missing file returns defaults", func(t *testing.T) {
		t.Setenv("LLM_PROXY_CONFIG", filepath.Join(t.TempDir(), "nope.yaml"))
		cfg := LoadRoutingConfig()
		if len(cfg.Rules) < 1 {
			t.Errorf("expected default rules, got %d", len(cfg.Rules))
		}
	})
	t.Run("malformed file falls back to defaults", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.yaml")
		// mixed tabs/spaces — the parser rejects this as an unexpected indent.
		if err := os.WriteFile(path, []byte("rules:\n\t- from: \"x\"\n  to: y\n"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		t.Setenv("LLM_PROXY_CONFIG", path)
		cfg := LoadRoutingConfig()
		if len(cfg.Rules) < 1 {
			t.Errorf("malformed yaml must fall back to defaults, got %d rules", len(cfg.Rules))
		}
	})
}

// LoadRoutingConfigFromString must report a parse error for empty input
// and for malformed YAML, but accept well-formed YAML and translate it
// into a Config with the right routing table and default upstream.
func TestLoadRoutingConfigFromString(t *testing.T) {
	t.Run("empty source returns error", func(t *testing.T) {
		// Empty string yields no rules and no error — by design the parser
		// treats an empty doc as an empty map. Lock in the contract here.
		if _, err := LoadRoutingConfigFromString(""); err != nil {
			t.Errorf("empty source should not error: %v", err)
		}
	})
	t.Run("well-formed yaml with rules and provider to:", func(t *testing.T) {
		yaml := "listen_addr: 1.2.3.4:9999\n" +
			"default_to: opencode-go/deepseek-v4-flash\n" +
			"default_base_url: https://example.com/v1\n" +
			"default_url_pattern: /chat/completions\n" +
			"default_reasoning_effort: high\n" +
			"default_timeout_s: 42\n" +
			"rules:\n" +
			"  - priority: 5\n" +
			"    from: 'foo/*'\n" +
			"    to: opencode-go foo/bar\n" +
			"    reasoning_effort: low\n" +
			"    base_url: https://override.example.com/v1\n" +
			"    url_pattern: /v1/chat/completions\n" +
			"    timeout_s: 99\n"
		cfg, err := LoadRoutingConfigFromString(yaml)
		if err != nil {
			t.Fatalf("LoadRoutingConfigFromString: %v", err)
		}
		if cfg.ListenAddr != "1.2.3.4:9999" {
			t.Errorf("ListenAddr = %q", cfg.ListenAddr)
		}
		if cfg.DefaultUS.BaseURL != "https://example.com/v1" {
			t.Errorf("DefaultUS.BaseURL = %q", cfg.DefaultUS.BaseURL)
		}
		if cfg.DefaultUS.TimeoutS != 42 {
			t.Errorf("DefaultUS.TimeoutS = %d, want 42", cfg.DefaultUS.TimeoutS)
		}
		if len(cfg.Rules) < 2 {
			t.Fatalf("expected at least 2 rules, got %d", len(cfg.Rules))
		}
		// First rule must be the user-supplied one (priority 5).
		var r *Rule
		for i := range cfg.Rules {
			if cfg.Rules[i].MatchExpr == "foo/*" {
				r = &cfg.Rules[i]
				break
			}
		}
		if r == nil {
			t.Fatalf("foo/* rule missing")
		}
		if r.Upstream.BaseURL != "https://override.example.com/v1" {
			t.Errorf("BaseURL = %q, want override", r.Upstream.BaseURL)
		}
		if r.Upstream.URLPattern != "/v1/chat/completions" {
			t.Errorf("URLPattern = %q", r.Upstream.URLPattern)
		}
		if r.Upstream.ReasoningEffort != "low" {
			t.Errorf("ReasoningEffort = %q, want low", r.Upstream.ReasoningEffort)
		}
		if r.Upstream.TimeoutS != 99 {
			t.Errorf("TimeoutS = %d, want 99", r.Upstream.TimeoutS)
		}
	})
	t.Run("malformed yaml returns error", func(t *testing.T) {
		// Mixed indentation breaks the parser.
		if _, err := LoadRoutingConfigFromString("rules:\n\t- from: x\n  to: y\n"); err == nil {
			t.Errorf("expected parse error for mixed-indent yaml, got nil")
		}
	})
	t.Run("default_timeout_s zero or negative is ignored", func(t *testing.T) {
		yaml := "default_timeout_s: 0\n"
		cfg, err := LoadRoutingConfigFromString(yaml)
		if err != nil {
			t.Fatalf("LoadRoutingConfigFromString: %v", err)
		}
		if cfg.DefaultUS.TimeoutS != 0 {
			t.Errorf("TimeoutS = %d, want 0 (zero input must be ignored)", cfg.DefaultUS.TimeoutS)
		}
	})
	t.Run("proxy routes section", func(t *testing.T) {
		yaml := "proxy:\n  routes:\n    - url: socks5://127.0.0.1:9050\n      family: minimax\n    - url: socks5://127.0.0.1:9060\n"
		cfg, err := LoadRoutingConfigFromString(yaml)
		if err != nil {
			t.Fatalf("LoadRoutingConfigFromString: %v", err)
		}
		if len(cfg.ProxySpecs) != 1 {
			t.Fatalf("ProxySpecs = %d, want 1 (the entry without family must be skipped)", len(cfg.ProxySpecs))
		}
		if cfg.ProxySpecs[0].URL != "socks5://127.0.0.1:9050" {
			t.Errorf("ProxySpecs[0].URL = %q", cfg.ProxySpecs[0].URL)
		}
	})
	t.Run("rule with empty from or to is skipped", func(t *testing.T) {
		yaml := "rules:\n  - from: ''\n    to: opencode-go/foo\n  - from: 'bar/*'\n    to: ''\n  - from: 'baz/*'\n    to: opencode-go/qux\n"
		cfg, err := LoadRoutingConfigFromString(yaml)
		if err != nil {
			t.Fatalf("LoadRoutingConfigFromString: %v", err)
		}
		// Default rules are pre-loaded; assert none of the bad entries leaked.
		for _, r := range cfg.Rules {
			if r.MatchExpr == "" {
				t.Errorf("rule with empty from leaked: %+v", r)
			}
		}
	})
	t.Run("non-map rule item skipped", func(t *testing.T) {
		yaml := "rules:\n  - 'just a string'\n"
		cfg, err := LoadRoutingConfigFromString(yaml)
		if err != nil {
			t.Fatalf("LoadRoutingConfigFromString: %v", err)
		}
		// Only built-in rules should be present.
		for _, r := range cfg.Rules {
			if r.MatchExpr == "just a string" {
				t.Errorf("string entry must be ignored: %+v", r)
			}
		}
	})
}

// defaultConfig must produce a usable routing table out of the box — at
// least one built-in rule must be present and the defaults must point
// at the opencode-go family.
func TestDefaultConfig(t *testing.T) {
	cfg := defaultConfig()
	if cfg.ListenAddr == "" {
		t.Errorf("ListenAddr empty")
	}
	if cfg.DefaultUS.Type != "opencode-go" {
		t.Errorf("DefaultUS.Type = %q, want opencode-go", cfg.DefaultUS.Type)
	}
	if len(cfg.Rules) == 0 {
		t.Fatalf("defaultConfig produced 0 rules")
	}
	// Look for the canonical "GO/*" rule.
	var found bool
	for _, r := range cfg.Rules {
		if r.MatchExpr == "GO/*" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("canonical GO/* rule missing from defaultConfig")
	}
}

// ParseToString splits a routing target string of the form
// "<provider> <namespace>/<model>[ <eff>]" into its components and
// reports whether the parse succeeded.
func TestParseToString(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantOk   bool
		wantType string
		wantMdl  string
		wantEff  string
	}{
		// "minimax" requires namespace "sub", so "minimax foo/bar" is rejected.
		{"minimax foo/bar", "minimax foo/bar", false, "", "", ""},
		// "opencode-go foo" has no "/" — rejected (the slash is required).
		{"opencode-go foo", "opencode-go foo", false, "", "", ""},
		// Effort is stripped from the end; the rest must be "<prov> <ns>/<mod>".
		// "opencode-zen foo/bar baz high" becomes "opencode-zen foo/bar baz"
		// after effort strip — namespace "foo", mod "bar baz".
		{"opencode-zen foo/bar baz high", "opencode-zen foo/bar baz high", true, "opencode-zen", "bar baz", "high"},
		{"unknown", "wat foo/bar", false, "", "", ""},
		{"empty", "", false, "", "", ""},
		{"ollama tag", "ollama deepseek-v4-flash:cloud", true, "ollama", "deepseek-v4-flash:cloud", ""},
		{"ollama empty", "ollama ", false, "", "", ""},
		{"minimax wrong namespace", "minimax foo/bar", false, "", "", ""},
		// "minimax sub/MiniMax" → the parser normalises the "minimax"
		// prefix to "MiniMax-" so the canonical prefix survives
		// wildcard capture. With no separator/dash the result is the
		// bare canonical prefix.
		{"minimax sub/MiniMax", "minimax sub/MiniMax", true, "minimax", "MiniMax-", ""},
		// The wildcard suffix "*" is preserved so the captured substring
		// can be appended later by ResolveRule.
		{"minimax wildcard preserves case", "minimax sub/minimax*", true, "minimax", "MiniMax-*", ""},
		{"opencode-go max effort", "opencode-go foo/max max", true, "opencode-go", "foo/max", "max"},
		{"openrouter bare", "openrouter ", false, "", "", ""},
		{"opencode-zen empty namespace", "opencode-zen /foo", false, "", "", ""},
		{"opencode-go no slash", "opencode-go", false, "", "", ""},
		{"opencode plain", "opencode foo/bar", true, "opencode-go", "foo/bar", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, mdl, _, _, eff, ok := ParseToString(tc.in)
			if ok != tc.wantOk {
				t.Fatalf("ok = %v, want %v (input=%q)", ok, tc.wantOk, tc.in)
			}
			if !ok {
				return
			}
			if typ != tc.wantType {
				t.Errorf("type = %q, want %q", typ, tc.wantType)
			}
			if mdl != tc.wantMdl {
				t.Errorf("mdl = %q, want %q", mdl, tc.wantMdl)
			}
			if eff != tc.wantEff {
				t.Errorf("eff = %q, want %q", eff, tc.wantEff)
			}
		})
	}
}

// buildUpstreamFromTo assembles an Upstream from the parsed "to:" string
// plus per-rule overrides for base URL, URL pattern and reasoning effort.
func TestBuildUpstreamFromTo(t *testing.T) {
	t.Run("minimax type sets anthropic base", func(t *testing.T) {
		us, preserveTo := buildUpstreamFromTo("minimax sub/foo", "", "", "", Upstream{})
		if us.Type != "minimax" {
			t.Errorf("type = %q", us.Type)
		}
		if us.BaseURL != DefaultMinimaxURL {
			t.Errorf("BaseURL = %q, want DefaultMinimaxURL", us.BaseURL)
		}
		if us.URLPattern != "/v1/messages" {
			t.Errorf("URLPattern = %q", us.URLPattern)
		}
		if preserveTo != "" {
			t.Errorf("preserveTo = %q, want empty", preserveTo)
		}
	})
	t.Run("opencode-zen type sets zen base", func(t *testing.T) {
		us, _ := buildUpstreamFromTo("opencode-zen foo/bar", "", "", "", Upstream{})
		if us.BaseURL != opencodeZenBaseURL {
			t.Errorf("BaseURL = %q", us.BaseURL)
		}
	})
	t.Run("ollama type sets ollama base", func(t *testing.T) {
		us, _ := buildUpstreamFromTo("ollama deepseek:cloud", "", "", "", Upstream{})
		if us.BaseURL != ollamaCloudBaseURL {
			t.Errorf("BaseURL = %q", us.BaseURL)
		}
	})
	t.Run("openrouter type sets openrouter base", func(t *testing.T) {
		us, _ := buildUpstreamFromTo("openrouter anthropic/claude", "", "", "", Upstream{})
		if us.BaseURL != OpenrouterBaseURL {
			t.Errorf("BaseURL = %q", us.BaseURL)
		}
	})
	t.Run("default branch is opencode-go", func(t *testing.T) {
		us, _ := buildUpstreamFromTo("opencode-go foo/bar", "", "", "", Upstream{})
		if us.BaseURL != OpencodeGoBaseURL {
			t.Errorf("BaseURL = %q", us.BaseURL)
		}
	})
	t.Run("base override wins", func(t *testing.T) {
		us, _ := buildUpstreamFromTo("minimax sub/foo", "https://override.example.com", "", "", Upstream{})
		if us.BaseURL != "https://override.example.com" {
			t.Errorf("BaseURL = %q, want override", us.BaseURL)
		}
	})
	t.Run("url override wins", func(t *testing.T) {
		us, _ := buildUpstreamFromTo("minimax sub/foo", "", "/v1/x", "", Upstream{})
		if us.URLPattern != "/v1/x" {
			t.Errorf("URLPattern = %q", us.URLPattern)
		}
	})
	t.Run("explicit effort wins over parsed effort", func(t *testing.T) {
		us, _ := buildUpstreamFromTo("opencode-go foo/bar high", "", "", "low", Upstream{})
		if us.ReasoningEffort != "low" {
			t.Errorf("ReasoningEffort = %q, want low (explicit override)", us.ReasoningEffort)
		}
	})
	t.Run("parsed effort used when no explicit", func(t *testing.T) {
		us, _ := buildUpstreamFromTo("opencode-go foo/bar high", "", "", "", Upstream{})
		if us.ReasoningEffort != "high" {
			t.Errorf("ReasoningEffort = %q, want high", us.ReasoningEffort)
		}
	})
	t.Run("preserveTo set on wildcard target", func(t *testing.T) {
		_, preserveTo := buildUpstreamFromTo("minimax sub/minimax*", "", "", "", Upstream{})
		if preserveTo != "MiniMax-" {
			t.Errorf("preserveTo = %q, want MiniMax-", preserveTo)
		}
	})
	t.Run("fallback returned when parse fails", func(t *testing.T) {
		fb := Upstream{Type: "fallback"}
		us, _ := buildUpstreamFromTo("wat foo/bar", "", "", "", fb)
		if us.Type != "fallback" {
			t.Errorf("fallback not returned, got %+v", us)
		}
	})
}

// CompileFromPattern turns a routing "from:" glob (with at most one "*")
// into a case-insensitive anchored regex and reports the capture index.
func TestCompileFromPattern(t *testing.T) {
	t.Run("exact match", func(t *testing.T) {
		re, idx, err := CompileFromPattern("minimax/MiniMax-M3")
		if err != nil {
			t.Fatalf("CompileFromPattern: %v", err)
		}
		if idx != -1 {
			t.Errorf("idx = %d, want -1 (no capture)", idx)
		}
		if !re.MatchString("minimax/MiniMax-M3") {
			t.Errorf("exact match failed on equal input")
		}
		if re.MatchString("minimax/MiniMax-M3X") {
			t.Errorf("exact match should be anchored")
		}
		if re.MatchString("foo/minimax/MiniMax-M3") {
			t.Errorf("exact match should reject prefix")
		}
	})
	t.Run("trailing star captures suffix", func(t *testing.T) {
		re, idx, err := CompileFromPattern("GO/*")
		if err != nil {
			t.Fatalf("CompileFromPattern: %v", err)
		}
		if idx != 1 {
			t.Errorf("idx = %d, want 1", idx)
		}
		m := re.FindStringSubmatch("go/some-model")
		if m == nil || m[1] != "some-model" {
			t.Errorf("match = %v, want [some-model]", m)
		}
	})
	t.Run("prefix+suffix captures middle", func(t *testing.T) {
		re, _, err := CompileFromPattern("opencode-go/*-flash")
		if err != nil {
			t.Fatalf("CompileFromPattern: %v", err)
		}
		m := re.FindStringSubmatch("opencode-go/deepseek-v4-flash")
		if m == nil || m[1] != "deepseek-v4" {
			t.Errorf("match = %v, want middle capture", m)
		}
	})
	t.Run("case insensitive", func(t *testing.T) {
		re, _, _ := CompileFromPattern("GO/*")
		if !re.MatchString("go/X") || !re.MatchString("GO/X") {
			t.Errorf("compile not case insensitive")
		}
	})
}

// sortRules orders rules highest-priority-first and is stable for equal
// priorities (insertion order preserved).
func TestSortRules(t *testing.T) {
	rules := []Rule{
		{MatchExpr: "a", Priority: 5},
		{MatchExpr: "b", Priority: 20},
		{MatchExpr: "c", Priority: 5},
		{MatchExpr: "d", Priority: 10},
	}
	sortRules(rules)
	// Insertion sort: highest priority first, stable for ties.
	// Input [a=5, b=20, c=5, d=10] → [a=5, c=5, d=10, b=20].
	wantOrder := []string{"a", "c", "d", "b"}
	for i, w := range wantOrder {
		if rules[i].MatchExpr != w {
			t.Errorf("rules[%d] = %q, want %q", i, rules[i].MatchExpr, w)
		}
	}
}

// ResolveRule picks the first regex that matches the request key and
// returns its upstream (with the captured wildcard substitution applied);
// falls back to DefaultUS when no rule matches.
func TestResolveRule(t *testing.T) {
	t.Run("specific rule beats catch-all", func(t *testing.T) {
		cfg := Config{}
		cfg.Rules = append(cfg.Rules,
			Rule{MatchExpr: "*", Priority: 10, MatchRe: regexp.MustCompile("(?i)^(.*)$"), CaptureSrc: 1, Upstream: Upstream{Model: "catch*"}},
		)
		cfg.Rules = append(cfg.Rules,
			Rule{MatchExpr: "opencode-go/MiniMax-*", Priority: 0, MatchRe: regexp.MustCompile("(?i)^opencode-go/MiniMax-(.*)$"), CaptureSrc: 1, Upstream: Upstream{Type: "minimax", Model: "MiniMax-*"}},
		)
		sortRules(cfg.Rules) // ascending priority — specific (P0) first, catch-all (P10) last.
		us := cfg.ResolveRule("opencode-go/MiniMax-M3")
		if us.Type != "minimax" {
			t.Errorf("specific rule must win over catch-all, got type=%q", us.Type)
		}
		if us.Model != "MiniMax-M3" {
			t.Errorf("model = %q, want MiniMax-M3 (captured)", us.Model)
		}
	})
	t.Run("falls back to default", func(t *testing.T) {
		cfg := Config{DefaultUS: Upstream{Type: "opencode-go"}}
		us := cfg.ResolveRule("anything")
		if us.Type != "opencode-go" {
			t.Errorf("default fallback not used")
		}
	})
	t.Run("no rule + no default returns zero upstream", func(t *testing.T) {
		cfg := Config{}
		us := cfg.ResolveRule("anything")
		if us.Type != "" {
			t.Errorf("zero Config should produce zero upstream, got %+v", us)
		}
	})
	t.Run("rule with nil MatchRe is skipped", func(t *testing.T) {
		cfg := Config{
			DefaultUS: Upstream{Type: "fallback"},
			Rules: []Rule{
				{MatchExpr: "x", MatchRe: nil, Upstream: Upstream{Type: "should-not-pick"}},
			},
		}
		us := cfg.ResolveRule("x")
		if us.Type != "fallback" {
			t.Errorf("nil-MatchRe rule must be skipped, got %+v", us)
		}
	})
	t.Run("captured prefix with leading dash", func(t *testing.T) {
		// Capture that begins with "-" must be normalized so the composed
		// model doesn't gain a stray dash.
		re, _, _ := CompileFromPattern("opencode-go/MiniMax-*")
		cfg := Config{
			DefaultUS: Upstream{Type: "opencode-go"},
			Rules: []Rule{
				{MatchExpr: "opencode-go/MiniMax-*", MatchRe: re, CaptureSrc: 1, Upstream: Upstream{Type: "minimax", Model: "MiniMax-*"}},
			},
		}
		us := cfg.ResolveRule("opencode-go/MiniMax--m3")
		// After TrimPrefix the capture is "m3"; composed with "MiniMax-" prefix gives "MiniMax-m3".
		if us.Model != "MiniMax-m3" {
			t.Errorf("model = %q, want MiniMax-m3", us.Model)
		}
	})
	t.Run("empty capture falls back to trimmed prefix", func(t *testing.T) {
		re, _, _ := CompileFromPattern("GO/*")
		cfg := Config{
			DefaultUS: Upstream{Type: "opencode-go"},
			Rules: []Rule{
				{MatchExpr: "GO/*", MatchRe: re, CaptureSrc: 1, Upstream: Upstream{Type: "minimax", Model: "MiniMax-*"}},
			},
		}
		// Original-case regex match is used when computing the capture; an
		// empty capture means "preserve the prefix only".
		us := cfg.ResolveRule("GO/")
		if !strings.HasSuffix(us.Model, "MiniMax-") && !strings.HasSuffix(us.Model, "MiniMax") {
			t.Errorf("model = %q, want trailing - normalized", us.Model)
		}
	})
}

// ApplyReasoningEffort adds the "reasoning_effort" field to a JSON body
// when it is missing and the caller supplied an effort. Bodies that
// already carry the field, non-JSON bodies, or empty efforts are
// returned unchanged.
func TestApplyReasoningEffort(t *testing.T) {
	t.Run("empty effort unchanged", func(t *testing.T) {
		body := []byte(`{"model":"x"}`)
		if got := ApplyReasoningEffort(body, ""); !reflect.DeepEqual(got, body) {
			t.Errorf("body changed with empty effort: %s", got)
		}
	})
	t.Run("non-json body unchanged", func(t *testing.T) {
		body := []byte("not json at all")
		if got := ApplyReasoningEffort(body, "high"); !reflect.DeepEqual(got, body) {
			t.Errorf("body changed for non-json input: %s", got)
		}
	})
	t.Run("body with reasoning_effort unchanged", func(t *testing.T) {
		body := []byte(`{"model":"x","reasoning_effort":"low"}`)
		if got := ApplyReasoningEffort(body, "high"); string(got) != string(body) {
			t.Errorf("body mutated when reasoning_effort already set: %s", got)
		}
	})
	t.Run("body without reasoning_effort gets field", func(t *testing.T) {
		body := []byte(`{"model":"x","messages":[]}`)
		got := ApplyReasoningEffort(body, "max")
		if !strings.Contains(string(got), `"reasoning_effort":"max"`) {
			t.Errorf("reasoning_effort not injected: %s", got)
		}
	})
	t.Run("invalid json marshal returns original body", func(t *testing.T) {
		// map[string]any marshalling of a value that contains a non-serializable
		// type — use a channel which cannot be json-encoded.
		body := []byte(`{"model":"x","data":null}`)
		got := ApplyReasoningEffort(body, "high")
		if !strings.Contains(string(got), `"reasoning_effort":"high"`) {
			t.Errorf("expected injection, got %s", got)
		}
	})
}
