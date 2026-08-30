package proxy

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// --- providers (failover counters; same shape as minimax-proxy) ---

type Provider struct {
	Name   string // friendly alias (e.g. "minimax-coding-plan", "opencode-go-1")
	Family string // "minimax" | "opencode-go" — used by routing/forwarding
	Key    string
	Stats  ProviderStats
	// cooldownUntil is a unixnano deadline (0 = none): after a recent
	// 429/401/403/400/timeout the key is skipped until the deadline passes,
	// then it re-enters rotation and is probed again — never forgotten.
	cooldownUntil atomic.Int64
}

type ProviderStats struct {
	Requests2xx      uint64 `json:"requests_2xx"`
	Requests429      uint64 `json:"requests_429"`
	RequestsOther    uint64 `json:"requests_other"`
	FailoverHits     uint64 `json:"failover_hits"`
	Last2xxNano      int64  `json:"last_2xx_unixnano,omitempty"`
	Last429Nano      int64  `json:"last_429_unixnano,omitempty"`
	LastFailoverNano int64  `json:"last_failover_unixnano,omitempty"`
}

func (s *ProviderStats) observe(status int) {
	switch {
	case status >= 200 && status < 300:
		atomic.AddUint64(&s.Requests2xx, 1)
		atomic.StoreInt64(&s.Last2xxNano, time.Now().UnixNano())
	case status == 429:
		atomic.AddUint64(&s.Requests429, 1)
		atomic.StoreInt64(&s.Last429Nano, time.Now().UnixNano())
	default:
		atomic.AddUint64(&s.RequestsOther, 1)
	}
}

func (s *ProviderStats) recordFailover() {
	atomic.AddUint64(&s.FailoverHits, 1)
	atomic.StoreInt64(&s.LastFailoverNano, time.Now().UnixNano())
}

type ProviderError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func LoadProviders() ([]Provider, error) {
	providers := []Provider{}

	// Minimax subscription — two distinct keys, must both be set.
	codingKey := os.Getenv("MINIMAX_CODING_PLAN_KEY")
	secondaryKey := os.Getenv("MINIMAX_KEY")
	if codingKey == "" || secondaryKey == "" {
		return nil, fmt.Errorf("MINIMAX_CODING_PLAN_KEY and MINIMAX_KEY must both be set")
	}
	if codingKey == secondaryKey {
		return nil, fmt.Errorf("MINIMAX_CODING_PLAN_KEY and MINIMAX_KEY are identical — the whole point of this proxy is to use two DIFFERENT keys")
	}
	providers = append(providers,
		Provider{Name: "minimax-coding-plan", Family: "minimax", Key: codingKey},
		Provider{Name: "minimax", Family: "minimax", Key: secondaryKey},
	)

	// opencode-go subscription — keys read from OPENCODE_GO_KEY_1..N. Blank
	// entries are skipped; bare numeric suffix is appended to the alias.
	for i := 1; i <= 16; i++ {
		k := os.Getenv(fmt.Sprintf("OPENCODE_GO_KEY_%d", i))
		if k == "" {
			continue
		}
		providers = append(providers, Provider{
			Name:   fmt.Sprintf("opencode-go-%d", i),
			Family: "opencode-go",
			Key:    k,
		})
	}

	// opencode Zen (free tier) — multi-key rotation. Keys read from
	// OPENCODE_ZEN_KEY_1..N; a bare OPENCODE_ZEN_KEY (no suffix) is
	// accepted as a single-key fallback for backward compatibility.
	// A missing key just hides the Zen family from routing.
	for i := 1; i <= 16; i++ {
		k := os.Getenv(fmt.Sprintf("OPENCODE_ZEN_KEY_%d", i))
		if k == "" {
			continue
		}
		providers = append(providers, Provider{
			Name:   fmt.Sprintf("opencode-zen-%d", i),
			Family: "opencode-zen",
			Key:    k,
		})
	}
	if zk := os.Getenv("OPENCODE_ZEN_KEY"); zk != "" {
		providers = append(providers, Provider{
			Name:   "opencode-zen",
			Family: "opencode-zen",
			Key:    zk,
		})
	}

	// Ollama Cloud — single key, no failover. Optional: a missing key
	// just hides the Ollama family from routing.
	if ok := os.Getenv("OLLAMA_API_KEY"); ok != "" {
		providers = append(providers, Provider{
			Name:   "ollama-cloud",
			Family: "ollama",
			Key:    ok,
		})
	}

	// OpenRouter (openrouter.ai) — keys read from OPENROUTER_KEY_1..N.
	// Blank entries are skipped; bare numeric suffix is appended to the
	// alias. The upstream serves both an OpenAI-compatible
	// (/api/v1/chat/completions) and an Anthropic-compatible
	// (/api/v1/messages) endpoint; we hit the Anthropic one so the
	// proxy stays drop-in for opencode's /v1/messages clients. OpenRouter
	// accepts both Authorization: Bearer and x-api-key, so we set both
	// (same belt-and-braces as ForwardMinimax).
	for i := 1; i <= 16; i++ {
		k := os.Getenv(fmt.Sprintf("OPENROUTER_KEY_%d", i))
		if k == "" {
			continue
		}
		providers = append(providers, Provider{
			Name:   fmt.Sprintf("openrouter-%d", i),
			Family: "openrouter",
			Key:    k,
		})
	}

	return providers, nil
}
