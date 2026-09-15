// Package synthetic implements role-routed model resolution for the
// llm-proxy. Instead of binding omp to a single model per role, the
// proxy exposes 10 synthetic model IDs (`synthetic/<role>`) and runs
// a per-role ordered ladder of upstream attempts whenever one is
// requested. The first 2xx response is streamed to the caller; any
// transient failure (400/401/403/404/429) advances to the next entry.
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
// opencode-go (paid subscription via OPENCODE_GO_KEY_N) — the funded paid
// primary that most ladders lead with; opencode-zen (free rows only) — no
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
func ladderDefault() []Target {
	return []Target{
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamOpenrouter, "dots-studio/dots-3-note-preview:free", "free-note"},
		{FamMinimax, "MiniMax-M3", "paid-minimax-sub"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

// ladderSmol leads with the free tier and ends on the funded paid primary.
//
// Every free row below was verified to serve on 2026-09-15 by reading the
// `model` field out of the response body (a bare 200 is not evidence —
// forward.go substitutes models silently). Order is static on purpose:
// free pools flip between 200 and 429 within the hour, so availability
// must not be encoded here. The trailing go entry is what keeps the role
// from hard-failing while the paid path is healthy.
//
// The openrouter/pareto-code row is not a dependable terminal: OpenRouter
// 402s it whenever the account balance cannot cover the request's
// max_tokens ("You requested up to 8000 tokens, but can only afford 662"),
// which is every real agent request. Reproduced directly against OR on
// 2026-09-15, and not proxy-side — the proxy never emits 402, it copies the
// upstream status. That is why the go entry, not the router, is last.
func ladderSmol() []Target {
	return []Target{
		{FamOpenrouter, "nvidia/nemotron-3.5-lightning:free", "free-fast"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamOpenrouter, "liquid/lfm-2.5-2.6b:free", "free-tiny"},
		{FamOpenrouter, "dots-studio/dots-3-note-preview:free", "free-note"},
		{FamOpenrouter, "openrouter/pareto-code", "free-router"},
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-terminal"},
	}
}

func ladderSlow() []Target {
	return []Target{
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-reasoning"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamMinimax, "MiniMax-M3", "paid-minimax-top"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}
func ladderVision() []Target {
	return []Target{
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamOpenrouter, "google/gemma-4-31b-it:free", "free-vision"},
		{FamOpenrouter, "google/gemma-4-26b-a4b-it:free", "free-vision-alt"},
		{FamOpenrouter, "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free", "free-omni"},
		// No minimax sub vision tier; OR auto-router is the paid
		// fallback for image tasks.
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

func ladderPlan() []Target {
	return []Target{
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamMinimax, "MiniMax-M3", "paid-minimax-top"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

func ladderDesigner() []Target {
	return []Target{
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamOpenrouter, "google/gemma-4-31b-it:free", "free-vision"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		// No minimax sub vision; OR router handles paid design work.
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}
func ladderCommit() []Target {
	return []Target{
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamOpenrouter, "poolside/laguna-xs-2.1:free", "free-swe-small"},
		{FamOpenrouter, "dots-studio/dots-3-note-preview:free", "free-format"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

func ladderTiny() []Target {
	return []Target{
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamOpenrouter, "liquid/lfm-2.5-2.6b:free", "free-tiny"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-tiny-coder"},
		{FamOpenrouter, "dots-studio/dots-3-note-preview:free", "free-tiny-general"},
		{FamOpenrouter, "google/gemma-4-26b-a4b-it:free", "free-tiny-vision"},
		{FamOpenrouter, "openrouter/free", "terminal-cheapest-router"},
	}
}

func ladderTask() []Target {
	return []Target{
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamMinimax, "MiniMax-M3", "paid-minimax-top"},
		{FamMinimax, "MiniMax-M2.7", "paid-minimax-mid"},
		{FamOpenrouter, "openrouter/pareto-code", "terminal-router"},
	}
}

func ladderAdvisor() []Target {
	return []Target{
		{FamOpencodeGo, "deepseek-v4.1-flash", "go-deepseek-primary"},
		{FamOpencodeGo, "glm-5.3-flash", "go-glm-fallback"},
		// The free anchor is a "pro"-tier model rather than one of the
		// small alts: bigger models follow the read-only advisor tool
		// schema cleanly (no spurious bash calls), which the previous
		// small free entries produced as "tool bash quarantined" errors
		// on the advisor transcript.
		{FamOpenrouter, "nex-agi/nex-n2.5-pro:free", "free-pro"},
		{FamOpenrouter, "cohere/north-mini-code:free", "free-code"},
		{FamOpenrouter, "dots-studio/dots-3-note-preview:free", "free-note"},
		// Diversity vs. default (paid-minimax anchor); OR routers
		// provide a different paid provider.
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
