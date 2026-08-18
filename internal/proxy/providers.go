package proxy

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// --- providers (failover counters) ---

type Provider struct {
	Name   string // friendly alias (e.g. "opencode-go-1")
	Family string // "opencode-go" | "opencode-zen" — used by routing/forwarding
	Key    string
	Stats  ProviderStats
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

	// opencode Zen (free tier) — single key, no failover. Optional: a
	// missing key just hides the Zen family from routing.
	if zk := os.Getenv("OPENCODE_ZEN_KEY"); zk != "" {
		providers = append(providers, Provider{
			Name:   "opencode-zen",
			Family: "opencode-zen",
			Key:    zk,
		})
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers configured: set OPENCODE_GO_KEY_1..N and/or OPENCODE_ZEN_KEY")
	}

	return providers, nil
}
