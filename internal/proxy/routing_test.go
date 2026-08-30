package proxy

import (
	"regexp"
	"strings"
	"testing"
)

// TestDecide_InvalidJSON confirms Decide does not panic on a non-JSON body
// and falls through to the default upstream.
func TestDecide_InvalidJSON(t *testing.T) {
	cfg := defaultConfig()
	body := []byte("not-json-at-all")
	rd := Decide(&cfg, body, "/v1/messages", "minimax")
	// With invalid JSON, model is empty; Decide falls into the
	// "hostProvider != "" && model == """ branch → key = "minimax/".
	// That key has no matching rule, so the default upstream wins.
	if rd.RuleName != "minimax/" {
		t.Fatalf("rule name = %q", rd.RuleName)
	}
	if string(rd.RewrittenBody) != string(body) {
		t.Fatalf("expected body preserved on invalid JSON, got %q", rd.RewrittenBody)
	}
	if rd.Upstream.Type != cfg.DefaultUS.Type {
		t.Fatalf("upstream type = %q (default %q)", rd.Upstream.Type, cfg.DefaultUS.Type)
	}
}

// TestDecide_ModelWithSlashUsesItAsKey: when the body model contains "/",
// it becomes the routing key verbatim (opencode-go style).
func TestDecide_ModelWithSlashUsesItAsKey(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"opencode-go/my-model"}`)
	rd := Decide(&cfg, body, "/v1/chat/completions", "opencode-go")
	if rd.RuleName != "opencode-go/my-model" {
		t.Fatalf("rule = %q", rd.RuleName)
	}
	if rd.Upstream.Type != "opencode-go" {
		t.Fatalf("upstream type = %q", rd.Upstream.Type)
	}
}

// TestDecide_HostProviderNoModelFallsBack: when the body has no model but
// hostProvider is set, the key is "<hostProvider>/" which has no rule; we
// fall back to default upstream.
func TestDecide_HostProviderNoModelFallsBack(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{}`)
	rd := Decide(&cfg, body, "/v1/chat/completions", "opencode-go")
	if rd.RuleName != "opencode-go/" {
		t.Fatalf("rule = %q", rd.RuleName)
	}
	if rd.Upstream.Type != cfg.DefaultUS.Type {
		t.Fatalf("upstream type = %q (default %q)", rd.Upstream.Type, cfg.DefaultUS.Type)
	}
}

// TestDecide_HostProviderWithModel: body has model, hostProvider set, no slash
// → key is "<hostProvider>/<model>".
func TestDecide_HostProviderWithModel(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"foo"}`)
	rd := Decide(&cfg, body, "/v1/messages", "minimax")
	if rd.RuleName != "minimax/foo" {
		t.Fatalf("rule = %q", rd.RuleName)
	}
}

// TestDecide_AnthropicShapeFlip: when the upstream is opencode-go with a
// /chat/completions URL pattern and the body has Anthropic-shape content
// parts, the URL pattern is flipped to /v1/messages.
func TestDecide_AnthropicShapeFlip(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"foo","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	rd := Decide(&cfg, body, "/v1/messages", "opencode-go")
	if !strings.HasSuffix(rd.Upstream.URLPattern, "/v1/messages") {
		t.Fatalf("URL pattern = %q", rd.Upstream.URLPattern)
	}
}

// TestDecide_NoAnthropicFlipOnChatCompletions: when request path is not
// /v1/messages, the URL pattern stays as /chat/completions.
func TestDecide_NoAnthropicFlipOnChatCompletions(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"foo","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	rd := Decide(&cfg, body, "/v1/chat/completions", "opencode-go")
	if !strings.HasSuffix(rd.Upstream.URLPattern, "/chat/completions") {
		t.Fatalf("URL pattern = %q", rd.Upstream.URLPattern)
	}
}

// TestDecide_ModelRewrite: when upstream.Model is set and differs from the
// body model, the body is rewritten with the new model.
func TestDecide_ModelRewrite(t *testing.T) {
	cfg := defaultConfig()
	cfg.Rules = append(cfg.Rules, Rule{
		Priority:  999,
		MatchExpr: "opencode-go/MiniMax-*",
		Upstream: Upstream{
			Type:       "opencode-go",
			BaseURL:    OpencodeGoBaseURL,
			URLPattern: "/chat/completions",
			Model:      "MiniMax-m3",
		},
		MatchRe:    regexp.MustCompile(`(?i)^opencode-go/(.*)$`),
		CaptureSrc: 1,
	})
	body := []byte(`{"model":"opencode-go/MiniMax-X"}`)
	rd := Decide(&cfg, body, "/v1/chat/completions", "opencode-go")
	if !strings.Contains(string(rd.RewrittenBody), `"model":"MiniMax-m3"`) {
		t.Fatalf("rewritten body = %s", rd.RewrittenBody)
	}
}

// TestDecide_PrefixStripOpencodeGo: opencode-go body model with
// "opencode-go/" prefix is stripped on the way out.
func TestDecide_PrefixStripOpencodeGo(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"opencode-go/some-model"}`)
	rd := Decide(&cfg, body, "/v1/chat/completions", "opencode-go")
	if !strings.Contains(string(rd.RewrittenBody), `"model":"some-model"`) {
		t.Fatalf("rewritten body = %s", rd.RewrittenBody)
	}
}

// TestDecide_PrefixStripOpencodeZen: opencode-zen body model with
// "opencode-zen/zen/" prefix is stripped on the way out.
func TestDecide_PrefixStripOpencodeZen(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"opencode-zen/zen/big-pickle"}`)
	rd := Decide(&cfg, body, "/v1/chat/completions", "opencode-zen")
	if !strings.Contains(string(rd.RewrittenBody), `"model":"big-pickle"`) {
		t.Fatalf("rewritten body = %s", rd.RewrittenBody)
	}
}
func TestDecide_OpenrouterKeepsSlash(t *testing.T) {
	cfg := defaultConfig()
	// Add a rule that catches "openrouter/<id>" so the openrouter prefix
	// strip branch is reached.
	cfg.Rules = append(cfg.Rules, Rule{
		Priority:  999,
		MatchExpr: "openrouter/*",
		Upstream: Upstream{
			Type:       "openrouter",
			BaseURL:    OpenrouterBaseURL,
			URLPattern: "/v1/messages",
		},
		MatchRe:    regexp.MustCompile(`(?i)^openrouter/(.*)$`),
		CaptureSrc: 1,
	})
	body := []byte(`{"model":"openrouter/anthropic/claude-3.5"}`)
	rd := Decide(&cfg, body, "/v1/messages", "openrouter")
	if !strings.Contains(string(rd.RewrittenBody), `"model":"anthropic/claude-3.5"`) {
		t.Fatalf("rewritten body = %s", rd.RewrittenBody)
	}
}

// TestDecide_ReasoningEffortApplied: when upstream has a reasoning effort
// set, the rewritten body carries it.
func TestDecide_ReasoningEffortApplied(t *testing.T) {
	cfg := defaultConfig()
	cfg.DefaultUS.ReasoningEffort = "low"
	body := []byte(`{"model":"foo"}`)
	rd := Decide(&cfg, body, "/v1/messages", "minimax")
	if !strings.Contains(string(rd.RewrittenBody), `"reasoning_effort":"low"`) {
		t.Fatalf("rewritten body = %s", rd.RewrittenBody)
	}
}

// TestDetectAnthropicShape covers all branches of detectAnthropicShape.

// TestDecide_DefaultKeyFromRequestPath: when both hostProvider and model are
// empty, Decide uses the request path as the routing key.
func TestDecide_DefaultKeyFromRequestPath(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"x":1}`) // no "model" field → model is empty
	rd := Decide(&cfg, body, "/v1/messages", "")
	if rd.RuleName != "/v1/messages" {
		t.Fatalf("rule = %q", rd.RuleName)
	}
}
func TestDetectAnthropicShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"invalid json", "not-json", false},
		{"empty", `{}`, false},
		{"no messages, has system", `{"system":"hi"}`, true},
		{"messages not array", `{"messages":"hi"}`, false},
		{"content string not array", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		{"content array, no type=text", `{"messages":[{"role":"user","content":[{"foo":1}]}]}`, false},
		{"content array with text", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, true},
		{"content array with image", `{"messages":[{"role":"user","content":[{"type":"image"}]}]}`, true},
		{"content array with tool_use", `{"messages":[{"role":"user","content":[{"type":"tool_use"}]}]}`, true},
		{"content array with tool_result", `{"messages":[{"role":"user","content":[{"type":"tool_result"}]}]}`, true},
		{"content array mixed non-text", `{"messages":[{"role":"user","content":[{"type":"foo"}]}]}`, false},
		{"messages entry not object", `{"messages":["hi"]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectAnthropicShape([]byte(tc.body))
			if got != tc.want {
				t.Fatalf("detectAnthropicShape(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
