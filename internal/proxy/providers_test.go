package proxy

import (
	"strconv"
	"testing"
)

// clearAllProviderEnv blanks every env var that LoadProviders reads so each
// subtest starts from a clean slate. Without this the ambient env leaks
// OPENCODE_GO_KEY_1 from the developer shell into every test.
func clearAllProviderEnv(t *testing.T) {
	t.Helper()
	for i := 1; i <= 16; i++ {
		t.Setenv("OPENCODE_GO_KEY_"+strconv.Itoa(i), "")
		t.Setenv("OPENCODE_ZEN_KEY_"+strconv.Itoa(i), "")
		t.Setenv("OPENROUTER_KEY_"+strconv.Itoa(i), "")
	}
	t.Setenv("OPENCODE_ZEN_KEY", "")
	t.Setenv("OLLAMA_API_KEY", "")
}

// LoadProviders refuses to start when the minimax pair is missing or
// identical — the pair is the canonical failover pair, so a missing key
// or two copies of the same key defeats the whole point of the proxy.
func TestLoadProvidersMissingMinimaxErrors(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "")
	t.Setenv("MINIMAX_KEY", "")
	if _, err := LoadProviders(); err == nil {
		t.Fatalf("LoadProviders must fail when both minimax keys are empty")
	}
}

func TestLoadProvidersIdenticalMinimaxErrors(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "same")
	t.Setenv("MINIMAX_KEY", "same")
	if _, err := LoadProviders(); err == nil {
		t.Fatalf("LoadProviders must fail when minimax keys are identical")
	}
}

// When only the minimax pair is set, exactly 2 providers come back
// (one for each alias) and no other family is present.
func TestLoadProvidersOnlyMinimax(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "coding")
	t.Setenv("MINIMAX_KEY", "secondary")
	ps, err := LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("got %d providers, want 2 (minimax pair only)", len(ps))
	}
	for i := range ps {
		if ps[i].Family != "minimax" {
			t.Errorf("providers[%d].Family = %q, want minimax", i, ps[i].Family)
		}
	}
}

// OPENCODE_GO_KEY_1..N: each present key produces one opencode-go
// provider, named "opencode-go-<n>".
func TestLoadProvidersMultipleGoKeys(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "a")
	t.Setenv("MINIMAX_KEY", "b")
	t.Setenv("OPENCODE_GO_KEY_1", "k1")
	t.Setenv("OPENCODE_GO_KEY_2", "k2")
	t.Setenv("OPENCODE_GO_KEY_3", "k3")
	ps, err := LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	// 2 minimax + 3 go = 5
	if len(ps) != 5 {
		t.Fatalf("got %d providers, want 5", len(ps))
	}
	wantNames := map[string]bool{
		"minimax-coding-plan": false,
		"minimax":             false,
		"opencode-go-1":       false,
		"opencode-go-2":       false,
		"opencode-go-3":       false,
	}
	for i := range ps {
		if _, ok := wantNames[ps[i].Name]; ok {
			wantNames[ps[i].Name] = true
		}
	}
	for name, seen := range wantNames {
		if !seen {
			t.Errorf("provider %q missing", name)
		}
	}
}

// Gaps in OPENCODE_GO_KEY_N numbering are silently skipped — the proxy
// never fails on a hole in the sequence, only on a hole in the minimax
// pair.
func TestLoadProvidersGoKeysSkipped(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "a")
	t.Setenv("MINIMAX_KEY", "b")
	t.Setenv("OPENCODE_GO_KEY_2", "k2") // gap at 1
	t.Setenv("OPENCODE_GO_KEY_5", "k5") // gap at 3, 4
	ps, err := LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	var gos []string
	for i := range ps {
		if ps[i].Family == "opencode-go" {
			gos = append(gos, ps[i].Name)
		}
	}
	if len(gos) != 2 || gos[0] != "opencode-go-2" || gos[1] != "opencode-go-5" {
		t.Fatalf("opencode-go providers = %v, want [opencode-go-2 opencode-go-5]", gos)
	}
}

// Bare OPENCODE_ZEN_KEY (no numeric suffix) produces a single provider
// named "opencode-zen" — backward-compat fallback for single-key Zen users.
func TestLoadProvidersZenBare(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "a")
	t.Setenv("MINIMAX_KEY", "b")
	t.Setenv("OPENCODE_ZEN_KEY", "bare")
	ps, err := LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	count := 0
	for i := range ps {
		if ps[i].Family == "opencode-zen" {
			count++
			if ps[i].Name != "opencode-zen" || ps[i].Key != "bare" {
				t.Errorf("provider[%d] = %s/%s", i, ps[i].Name, ps[i].Key)
			}
		}
	}
	if count != 1 {
		t.Fatalf("got %d opencode-zen providers, want 1", count)
	}
}

// Numbered OPENCODE_ZEN_KEY_N produces one provider per present key.
func TestLoadProvidersZenNumbered(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "a")
	t.Setenv("MINIMAX_KEY", "b")
	t.Setenv("OPENCODE_ZEN_KEY_1", "z1")
	t.Setenv("OPENCODE_ZEN_KEY_3", "z3")
	ps, err := LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	var zen []string
	for i := range ps {
		if ps[i].Family == "opencode-zen" {
			zen = append(zen, ps[i].Name)
		}
	}
	if len(zen) != 2 || zen[0] != "opencode-zen-1" || zen[1] != "opencode-zen-3" {
		t.Fatalf("opencode-zen providers = %v, want [opencode-zen-1 opencode-zen-3]", zen)
	}
}

// Both bare + numbered set produces N+1 providers.
func TestLoadProvidersZenNumberedPlusBare(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "a")
	t.Setenv("MINIMAX_KEY", "b")
	t.Setenv("OPENCODE_ZEN_KEY", "bare")
	t.Setenv("OPENCODE_ZEN_KEY_1", "z1")
	t.Setenv("OPENCODE_ZEN_KEY_2", "z2")
	ps, err := LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	var zen []string
	for i := range ps {
		if ps[i].Family == "opencode-zen" {
			zen = append(zen, ps[i].Name)
		}
	}
	if len(zen) != 3 {
		t.Fatalf("got %d zen providers, want 3 (numbered-1, numbered-2, bare)", len(zen))
	}
	hasBare := false
	for _, n := range zen {
		if n == "opencode-zen" {
			hasBare = true
		}
	}
	if !hasBare {
		t.Errorf("bare 'opencode-zen' missing from %v", zen)
	}
}

// OLLAMA_API_KEY set adds a single ollama provider.
func TestLoadProvidersOllama(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "a")
	t.Setenv("MINIMAX_KEY", "b")
	t.Setenv("OLLAMA_API_KEY", "ok")
	ps, err := LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	count := 0
	for i := range ps {
		if ps[i].Family == "ollama" {
			count++
			if ps[i].Name != "ollama-cloud" {
				t.Errorf("provider[%d] = %s, want Name=ollama-cloud", i, ps[i].Name)
			}
		}
	}
	if count != 1 {
		t.Fatalf("got %d ollama providers, want 1", count)
	}
}

// OLLAMA_API_KEY absent — no ollama providers.
func TestLoadProvidersOllamaMissing(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "a")
	t.Setenv("MINIMAX_KEY", "b")
	ps, err := LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	for i := range ps {
		if ps[i].Family == "ollama" {
			t.Errorf("unexpected ollama provider: %s/%s", ps[i].Name, ps[i].Key)
		}
	}
}

// OPENROUTER_KEY_N: gaps in numbering are skipped silently and each
// present key produces one provider named "openrouter-<n>".
func TestLoadProvidersOpenrouter(t *testing.T) {
	clearAllProviderEnv(t)
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "a")
	t.Setenv("MINIMAX_KEY", "b")
	t.Setenv("OPENROUTER_KEY_1", "r1")
	t.Setenv("OPENROUTER_KEY_4", "r4") // gap at 2, 3
	t.Setenv("OPENROUTER_KEY_5", "r5")
	ps, err := LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	var ors []string
	for i := range ps {
		if ps[i].Family == "openrouter" {
			ors = append(ors, ps[i].Name)
		}
	}
	if len(ors) != 3 || ors[0] != "openrouter-1" || ors[1] != "openrouter-4" || ors[2] != "openrouter-5" {
		t.Fatalf("openrouter providers = %v, want [openrouter-1 openrouter-4 openrouter-5]", ors)
	}
}

// observe() classifies an upstream HTTP status into one of three
// buckets: 2xx, 429, or "other" — and stamps a per-bucket "last seen"
// timestamp. All three branches must be exercised.
func TestLoadProvidersObserveAllBranches(t *testing.T) {
	var s ProviderStats
	// 2xx
	s.observe(200)
	if s.Requests2xx != 1 || s.Requests429 != 0 || s.RequestsOther != 0 {
		t.Errorf("after 200: %+v", s)
	}
	if s.Last2xxNano == 0 {
		t.Errorf("Last2xxNano not stamped")
	}
	// 429
	s.observe(429)
	if s.Requests429 != 1 || s.Requests2xx != 1 {
		t.Errorf("after 429: %+v", s)
	}
	if s.Last429Nano == 0 {
		t.Errorf("Last429Nano not stamped")
	}
	// other (5xx)
	s.observe(500)
	if s.RequestsOther != 1 {
		t.Errorf("after 500: %+v", s)
	}
	// 3xx also falls into "other"
	s.observe(301)
	if s.RequestsOther != 2 {
		t.Errorf("after 301: %+v", s)
	}
	// 4xx other than 429 → "other"
	s.observe(503)
	s.observe(400)
	if s.RequestsOther != 4 {
		t.Errorf("after 503+400: %+v", s)
	}
}

// recordFailover bumps the FailoverHits counter and stamps LastFailoverNano.
func TestLoadProvidersRecordFailover(t *testing.T) {
	var s ProviderStats
	s.recordFailover()
	if s.FailoverHits != 1 {
		t.Errorf("after recordFailover: %+v", s)
	}
	if s.LastFailoverNano == 0 {
		t.Errorf("LastFailoverNano not stamped")
	}
	s.recordFailover()
	if s.FailoverHits != 2 {
		t.Errorf("after 2x recordFailover: %+v", s)
	}
}
