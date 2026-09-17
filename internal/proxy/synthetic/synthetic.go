// Package synthetic implements role-routed model resolution for the
// llm-proxy. Instead of binding omp to a single model per role, the
// proxy exposes 10 synthetic model IDs (`synthetic/<role>`) and runs
// a per-role ordered ladder of upstream attempts whenever one is
// requested. Every ladder is free-first and opens with the same stealth
// hop, stealth/union-alpha; the reasoning roles (plan, advisor, slow)
// skip the remaining free group and fall straight to a paid
// deepseek-v4.1-flash anchor. The first 2xx response is streamed to the
// caller; any transient failure (400/401/403/404/429) advances to the
// next entry.
//
// This keeps the role policy in one place (here + the compiled-in
// ladders below) and lets omp reference roles by a single stable
// model ID without exposing internal failover chains in
// ~/.omp/agent/config.yml.
//
// The package is decoupled from the rest of the proxy by emitting
// only the (family, model-id) target for a single ladder attempt;
// the caller (proxy.ForwardSynthetic in forwarding.go) owns the
// HTTP round-trip and key rotation per attempt. That separation
// means we never touch sensitive code paths in forward.go.
package synthetic

import (
	"sort"
	"strings"
	"sync"
)

// Family names — must match Upstream.Type values in proxy config:
// opencode-go (paid subscription via OPENCODE_GO_KEY_N) — a paid row: it
// closes the free group of every non-reasoning role and is the anchor row
// of the reasoning roles; opencode-zen (free rows only) — no
// ladder references it now, because the zen keys are the same credentials
// as the go keys and the free tier is 403-gated server-side; minimax (sub
// MINIMAX_CODING_PLAN_KEY/MINIMAX_KEY, expires 2026-09-09) — its keys are
// 429 today and the rows are kept as-is; openrouter (paid + free).
const (
	FamOpencodeZen = "opencode-zen"
	FamOpencodeGo  = "opencode-go"
	FamMinimax     = "minimax"
	FamOpenrouter  = "openrouter"
	FamOllama      = "ollama"
)

// Roles — the omp model-role surface.
const (
	RoleDefault  = "default"
	RoleSmol     = "smol"
	RoleSlow     = "slow"
	RoleVision   = "vision"
	RolePlan     = "plan"
	RoleDesigner = "designer"
	RoleCommit   = "commit"
	RoleTiny     = "tiny"
	RoleTask     = "task"
	RoleAdvisor  = "advisor"
)

// AllRoles is the canonical ordered list of supported role names.
var AllRoles = []string{
	RoleDefault, RoleSmol, RoleSlow, RoleVision, RolePlan,
	RoleDesigner, RoleCommit, RoleTiny, RoleTask, RoleAdvisor,
}

// Target is one ladder step: route (family, model-id) where model-id is
// the upstream wire-format name (e.g. "<vendor>/<model>:free" for
// openrouter, "<model>" bare for opencode-go).
type Target struct {
	Family string // matches proxy Upstream.Type
	Model  string // wire-format model id; OR uses "<vendor>/<name>[:free]", go uses bare names
	// Reason is a short tag for log lines (e.g. "free-stable", "paid-top").
	Reason string
}

// Resolve returns the static ladder for a role. Role name is matched
// case-insensitively; unknown roles return nil.
func Resolve(role string) []Target {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case RoleDefault:
		return ladderDefault()
	case RoleSmol:
		return ladderSmol()
	case RoleSlow:
		return ladderSlow()
	case RoleVision:
		return ladderVision()
	case RolePlan:
		return ladderPlan()
	case RoleDesigner:
		return ladderDesigner()
	case RoleCommit:
		return ladderCommit()
	case RoleTiny:
		return ladderTiny()
	case RoleTask:
		return ladderTask()
	case RoleAdvisor:
		return ladderAdvisor()
	default:
		return nil
	}
}

// IsSyntheticRole reports whether model is one of the proxy's
// synthetic IDs. omp uses shape `llm-proxy/synthetic/<role>` after
// omp prepends its own provider prefix.
func IsSyntheticModel(model string) bool {
	// omp prepends "synthetic/" or "llm-proxy/synthetic/" depending on
	// resolution context — accept both for robustness.
	m := strings.TrimPrefix(model, "llm-proxy/")
	m = strings.TrimPrefix(m, "synthetic/")
	for _, r := range AllRoles {
		if m == r {
			return true
		}
	}
	return false
}

// Union Alpha has no ":free" suffix but is free upstream (pricing 0/0).
func ladderDefault() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamOpenrouter, "dots-studio/dots-3-note-preview:free", "free-note"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamMinimax, "MiniMax-M3", "paid-minimax-sub"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

// Free rows other than Union Alpha were verified on 2026-09-15 by reading the
// `model` field out of the response body (a bare 200 is not evidence —
// forward.go substitutes models silently). Order is static on purpose:
// free pools flip between 200 and 429 within the hour, so availability
// must not be encoded here. Paid rows follow the free group in their
// existing order; the trailing go entry is what keeps the role from
// hard-failing while the paid path is healthy.
//
// No openrouter/pareto-code row: it is a PAID OR router, this account has no
// OR credit and will not get any, and OR 402s it on any request whose
// max_tokens exceeds the balance ("You requested up to 8000 tokens, but can
// only afford 662") — i.e. every real agent request. A row that can never
// serve is a wasted round-trip, not a fallback. Paid fallback here means the
// funded go subscription, which is what the last row is.
func ladderSmol() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpenrouter, "nvidia/nemotron-3.5-lightning:free", "free-fast"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamOpenrouter, "liquid/lfm-2.5-2.6b:free", "free-tiny"},
		{FamOpenrouter, "dots-studio/dots-3-note-preview:free", "free-note"},
		{FamOpenrouter, "poolside/laguna-s-2.1:free", "free-swe-tail"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-terminal"},
	}
}

func ladderSlow() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "paid-deepseek-reasoning"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamMinimax, "MiniMax-M3", "paid-minimax-top"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}
func ladderVision() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpenrouter, "google/gemma-4-31b-it:free", "free-vision"},
		{FamOpenrouter, "google/gemma-4-26b-a4b-it:free", "free-vision-alt"},
		{FamOpenrouter, "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free", "free-omni"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		// No minimax sub vision tier; OR auto-router is the paid
		// fallback for image tasks.
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

func ladderPlan() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "paid-deepseek-reasoning"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamMinimax, "MiniMax-M3", "paid-minimax-top"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

func ladderDesigner() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpenrouter, "google/gemma-4-31b-it:free", "free-vision"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		// No minimax sub vision; OR router handles paid design work.
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}
func ladderCommit() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamOpenrouter, "poolside/laguna-xs-2.1:free", "free-swe-small"},
		{FamOpenrouter, "dots-studio/dots-3-note-preview:free", "free-format"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

func ladderTiny() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpenrouter, "liquid/lfm-2.5-2.6b:free", "free-tiny"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-tiny-coder"},
		{FamOpenrouter, "dots-studio/dots-3-note-preview:free", "free-tiny-general"},
		{FamOpenrouter, "google/gemma-4-26b-a4b-it:free", "free-tiny-vision"},
		{FamOpenrouter, "openrouter/free", "free-router"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
	}
}

func ladderTask() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamMinimax, "MiniMax-M3", "paid-minimax-top"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

// Advisor retains two paid OR routers for provider diversity.
func ladderAdvisor() []Target {
	return []Target{
		{FamOpenrouter, "stealth/union-alpha", "free-union-alpha-default"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "paid-deepseek-reasoning"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
		{FamOpenrouter, "openrouter/auto-beta", "terminal-router-strong"},
	}
}

// CatalogIDs lists the 10 synthetic IDs as they appear in the
// proxy's /v1/models response (omp prepends its own "llm-proxy/"
// provider prefix when displaying).
func CatalogIDs() []string {
	ids := make([]string, 0, len(AllRoles))
	for _, r := range AllRoles {
		ids = append(ids, "llm-proxy/synthetic/"+r)
	}
	sort.Strings(ids)
	return ids
}

// --- cached ladder for hot path ---

var (
	cacheMu sync.RWMutex
	cache   = map[string][]Target{}
)

// Cached returns a stable copy of the ladder for `role`. The first
// lookup clones the static slice into the cache; subsequent lookups
// return the cached slice (cheap). The returned slice MUST NOT be
// mutated by callers; defensive copies are not issued because that
// would defeat the cache.
func Cached(role string) []Target {
	cacheMu.RLock()
	t, ok := cache[role]
	cacheMu.RUnlock()
	if ok {
		return t
	}
	resolved := Resolve(role)
	if resolved == nil {
		return nil
	}
	cacheMu.Lock()
	cache[role] = resolved
	cacheMu.Unlock()
	return resolved
}
