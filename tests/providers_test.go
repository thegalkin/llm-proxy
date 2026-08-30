package proxy_test

import (
	"testing"

	"llm-proxy/internal/proxy"
)

// LoadProviders reads env vars and refuses to run when the minimax pair
// is missing/identical; the zen/openrouter/opencode-go branches skip
// silently on missing keys. These tests assert the silent-skip contract
// and the multi-key rotation shape (OPENCODE_ZEN_KEY_1..N + a bare
// OPENCODE_ZEN_KEY fallback for backwards compatibility).

func setMinimaxPair(t *testing.T, key1, key2 string) {
	t.Helper()
	t.Setenv("MINIMAX_CODING_PLAN_KEY", key1)
	t.Setenv("MINIMAX_KEY", key2)
}

func TestLoadProvidersZenMultiKeyRotation(t *testing.T) {
	setMinimaxPair(t, "minimax-a", "minimax-b")
	t.Setenv("OPENCODE_ZEN_KEY", "")
	// Gap at 4: must be silently skipped, not failed.
	t.Setenv("OPENCODE_ZEN_KEY_1", "zen-one")
	t.Setenv("OPENCODE_ZEN_KEY_2", "zen-two")
	t.Setenv("OPENCODE_ZEN_KEY_3", "zen-three")
	t.Setenv("OPENCODE_ZEN_KEY_5", "zen-five")
	providers, err := proxy.LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	var zen []proxy.Provider
	for _, p := range providers {
		if p.Family == "opencode-zen" {
			zen = append(zen, p)
		}
	}
	if got := len(zen); got != 4 {
		t.Fatalf("opencode-zen providers = %d, want 4 (1,2,3,5 — gap at 4 silently skipped)", got)
	}
	wantNames := []string{"opencode-zen-1", "opencode-zen-2", "opencode-zen-3", "opencode-zen-5"}
	for i, p := range zen {
		if p.Name != wantNames[i] {
			t.Errorf("zen[%d].Name = %q, want %q", i, p.Name, wantNames[i])
		}
		if p.Key == "" {
			t.Errorf("zen[%d].Key empty", i)
		}
		if p.Name == "opencode-zen-4" {
			t.Errorf("zen includes opencode-zen-4 but env was empty")
		}
	}
}

func TestLoadProvidersZenBareFallback(t *testing.T) {
	setMinimaxPair(t, "minimax-a", "minimax-b")
	t.Setenv("OPENCODE_ZEN_KEY", "bare-key")
	for i := 1; i <= 16; i++ {
		t.Setenv("OPENCODE_ZEN_KEY_"+itoa(i), "")
	}
	providers, err := proxy.LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders: %v", err)
	}
	var zen []proxy.Provider
	for _, p := range providers {
		if p.Family == "opencode-zen" {
			zen = append(zen, p)
		}
	}
	if len(zen) != 1 || zen[0].Name != "opencode-zen" || zen[0].Key != "bare-key" {
		t.Fatalf("expected single bare opencode-zen provider; got %+v", zen)
	}
}

func TestLoadProvidersZenMissingAll(t *testing.T) {
	setMinimaxPair(t, "minimax-a", "minimax-b")
	t.Setenv("OPENCODE_ZEN_KEY", "")
	for i := 1; i <= 16; i++ {
		t.Setenv("OPENCODE_ZEN_KEY_"+itoa(i), "")
	}
	providers, err := proxy.LoadProviders()
	if err != nil {
		t.Fatalf("LoadProviders must NOT fail when all zen keys are missing; got %v", err)
	}
	for _, p := range providers {
		if p.Family == "opencode-zen" {
			t.Errorf("unexpected zen provider: %+v", p)
		}
	}
}

func TestLoadProvidersRequiresMinimaxKeys(t *testing.T) {
	// The minimax pair is the only required family — proxy MUST refuse
	// to start without it because the canonical minimax/M3 forwarding
	// path has no fallback.
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "")
	t.Setenv("MINIMAX_KEY", "")
	_, err := proxy.LoadProviders()
	if err == nil {
		t.Fatalf("LoadProviders succeeded with empty minimax keys; must error out")
	}
}

func TestLoadProvidersRejectsIdenticalMinimaxKeys(t *testing.T) {
	t.Setenv("MINIMAX_CODING_PLAN_KEY", "same-key")
	t.Setenv("MINIMAX_KEY", "same-key")
	_, err := proxy.LoadProviders()
	if err == nil {
		t.Fatalf("LoadProviders succeeded with identical minimax keys; failover is pointless")
	}
}

// itoa is a stdlib-free helper so the test file does not pull strconv.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
