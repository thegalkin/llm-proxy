package proxy_test

import (
	"testing"

	"llm-proxy/internal/proxy"
)

func TestParseYAMLSimpleMap(t *testing.T) {
	src := `
listen_addr: "127.0.0.1:9443"   # comment
default_to: opencode-go/deepseek-v4-flash low
retries: 3
verbose: true
missing: null
`
	root, err := proxy.ParseYAML(src)
	if err != nil {
		t.Fatalf("parseYAML: %v", err)
	}
	if got := root["listen_addr"]; got != "127.0.0.1:9443" {
		t.Errorf("listen_addr = %v, want 127.0.0.1:9443", got)
	}
	if got := root["default_to"]; got != "opencode-go/deepseek-v4-flash low" {
		t.Errorf("default_to = %v", got)
	}
	if got := root["retries"]; got != int64(3) {
		t.Errorf("retries = %v (%T), want int64(3)", got, got)
	}
	if got := root["verbose"]; got != true {
		t.Errorf("verbose = %v, want true", got)
	}
	if got := root["missing"]; got != nil {
		t.Errorf("missing = %v, want nil", got)
	}
}

func TestParseYAMLRules(t *testing.T) {
	src := `
rules:
  - from: "minimax/*"
    to: minimax sub/minimax*
    priority: 10
  - from: opencode-go/kimi-k3
    to: opencode-go/deepseek-v4-flash
    reasoning_effort: max
`
	root, err := proxy.ParseYAML(src)
	if err != nil {
		t.Fatalf("parseYAML: %v", err)
	}
	rules, ok := root["rules"].([]any)
	if !ok {
		t.Fatalf("rules = %T, want []any", root["rules"])
	}
	if len(rules) != 2 {
		t.Fatalf("len(rules) = %d, want 2", len(rules))
	}
	first, ok := rules[0].(map[string]any)
	if !ok {
		t.Fatalf("rules[0] = %T, want map[string]any", rules[0])
	}
	if first["from"] != "minimax/*" {
		t.Errorf("from = %v, want minimax/*", first["from"])
	}
	if first["to"] != "minimax sub/minimax*" {
		t.Errorf("to = %v", first["to"])
	}
	if first["priority"] != int64(10) {
		t.Errorf("priority = %v (%T), want int64(10)", first["priority"], first["priority"])
	}
	second, ok := rules[1].(map[string]any)
	if !ok {
		t.Fatalf("rules[1] = %T, want map[string]any", rules[1])
	}
	if second["reasoning_effort"] != "max" {
		t.Errorf("reasoning_effort = %v, want max", second["reasoning_effort"])
	}
}

func TestParseYAMLQuotedHash(t *testing.T) {
	src := `name: "a # b"
bare: value
`
	root, err := proxy.ParseYAML(src)
	if err != nil {
		t.Fatalf("parseYAML: %v", err)
	}
	if root["name"] != "a # b" {
		t.Errorf("name = %v, want %q (hash inside quotes must survive)", root["name"], "a # b")
	}
	if root["bare"] != "value" {
		t.Errorf("bare = %v, want value", root["bare"])
	}
}

func TestParseYAMLNestedMap(t *testing.T) {
	src := `
outer:
  inner:
    key: val
  sibling: 1
after: 2
`
	root, err := proxy.ParseYAML(src)
	if err != nil {
		t.Fatalf("parseYAML: %v", err)
	}
	outer, ok := root["outer"].(map[string]any)
	if !ok {
		t.Fatalf("outer = %T", root["outer"])
	}
	inner, ok := outer["inner"].(map[string]any)
	if !ok {
		t.Fatalf("inner = %T", outer["inner"])
	}
	if inner["key"] != "val" {
		t.Errorf("inner.key = %v, want val", inner["key"])
	}
	if outer["sibling"] != int64(1) {
		t.Errorf("outer.sibling = %v", outer["sibling"])
	}
	if root["after"] != int64(2) {
		t.Errorf("after = %v", root["after"])
	}
}

func TestParseYAMLErrors(t *testing.T) {
	if _, err := proxy.ParseYAML("key: |\n  block\n"); err == nil {
		t.Error("block scalar should be rejected")
	}
	if _, err := proxy.ParseYAML("  unexpected indent\n"); err == nil {
		t.Error("unexpected indent should be rejected")
	}
}
