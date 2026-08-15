package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// registerRoutes wires all HTTP endpoints onto mux.
func RegisterRoutes(mux *http.ServeMux, cfg *Config, providers []Provider) {
	mux.HandleFunc("/v1/messages", HandleMessages(cfg, providers))
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions(cfg, providers))
	mux.HandleFunc("/healthz", HandleHealthz(cfg, providers))
	mux.HandleFunc("/v1/models", HandleModels(cfg))
	mux.HandleFunc("/admin/limits", HandleLimits(cfg, providers))
}

// --- POST /v1/messages : Anthropic-shape, hostProvider comes from URL ---

func HandleMessages(cfg *Config, providers []Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.Body.Close()
		decision := Decide(cfg, body, r.URL.Path, "minimax")
		w.Header().Set(HeaderRoutingTag, decision.Upstream.Type+":"+decision.RuleName)
		forward(cfg, w, r, decision.RewrittenBody, decision, providers)
	}
}

// --- POST /v1/chat/completions : OpenAI-shape, hostProvider comes from URL ---

func handleChatCompletions(cfg *Config, providers []Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.Body.Close()
		// opencode-go sends these with provider/model in the body's
		// "model" field (e.g. "opencode/claude-sonnet-4-6" or
		// "opencode-go/minimax-m3"). The routing decision parses that
		// and selects the matching upstream.
		decision := Decide(cfg, body, r.URL.Path, "opencode-go")
		w.Header().Set(HeaderRoutingTag, decision.Upstream.Type+":"+decision.RuleName)
		forward(cfg, w, r, decision.RewrittenBody, decision, providers)
	}
}

// --- GET /healthz ---

func HandleHealthz(cfg *Config, providers []Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","rules":%d,"providers":%d,"default_type":%q}`,
			len(cfg.Rules), len(providers), cfg.DefaultUS.Type)
	}
}

// --- GET /v1/models : OpenAI-style model list for provider UIs ---

func HandleModels(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		seen := map[string]bool{}
		models := make([]map[string]any, 0, 96)
		add := func(id string) {
			if id == "" || seen[id] {
				return
			}
			seen[id] = true
			models = append(models, map[string]any{"id": id, "object": "model", "owned_by": "llm-proxy"})
		}
		for _, rl := range cfg.Rules {
			m := rl.Upstream.Model
			if m == "" || m == "*" || strings.HasSuffix(m, "*") {
				continue
			}
			if rl.Upstream.Type == "opencode-go" {
				add("llm-proxy/GO/" + m)
			} else {
				add("llm-proxy/" + rl.Upstream.Type + "/" + m)
			}
		}
		if cfg.DefaultUS.Model != "" && cfg.DefaultUS.Model != "*" {
			add("llm-proxy/GO/" + cfg.DefaultUS.Model)
		}
		zenModels := []string{
			"big-pickle", "claude-fable-5", "claude-haiku-4-5", "claude-opus-4-1",
			"claude-opus-4-5", "claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8",
			"claude-opus-5", "claude-sonnet-4", "claude-sonnet-4-5", "claude-sonnet-4-6",
			"claude-sonnet-5", "deepseek-v4-flash", "deepseek-v4-flash-free", "deepseek-v4-pro",
			"gemini-3.1-pro", "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.6-flash",
			"gemini-3-flash", "glm-5", "glm-5.1", "glm-5.2", "gpt-5", "gpt-5.1",
			"gpt-5.1-codex", "gpt-5.1-codex-max", "gpt-5.1-codex-mini", "gpt-5.2",
			"gpt-5.2-codex", "gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.4",
			"gpt-5.4-mini", "gpt-5.4-nano", "gpt-5.4-pro", "gpt-5.5", "gpt-5.5-pro",
			"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5-codex", "gpt-5-nano",
			"grok-4.5", "grok-build-0.1", "kimi-k2.5", "kimi-k2.6", "kimi-k2.7-code",
			"kimi-k3", "laguna-s-2.1-free", "ling-3.0-flash-free", "longcat-2.0-free",
			"mimo-v2.5-free", "minimax-m2.5", "minimax-m2.7", "minimax-m3",
			"nemotron-3-ultra-free", "north-mini-code-free", "qwen3.5-plus", "qwen3.6-plus",
		}
		for _, m := range zenModels {
			add("llm-proxy/opencode-zen/zen/" + m)
		}
		for _, id := range []string{
			"llm-proxy/GO/deepseek-v4-flash",
			"llm-proxy/GO/minimax-m3",
		} {
			add(id)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
	}
}

// --- GET /admin/limits : per-provider quota report ---

func HandleLimits(cfg *Config, providers []Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()

		results := make([]ProviderReport, len(providers))
		for i, p := range providers {
			results[i] = ProbeProviderQuota(ctx, p)
		}

		out := map[string]any{
			"uptime_unix": time.Now().Unix(),
			"providers":   results,
			"rules_count": len(cfg.Rules),
			"default":     cfg.DefaultUS.Type,
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	}
}

type quotaWindow struct {
	PctRemaining    *int   `json:"pct_remaining,omitempty"`
	TotalCount      *int64 `json:"total_count,omitempty"`
	RemainingCount  *int64 `json:"remaining_count,omitempty"`
	ConsumedCount   *int64 `json:"consumed_count,omitempty"`
	ResetInS        *int64 `json:"reset_in_s,omitempty"`
	Status          *int   `json:"status,omitempty"`
	WindowStartUnix *int64 `json:"window_start_unix,omitempty"`
	WindowEndUnix   *int64 `json:"window_end_unix,omitempty"`
}

type providerQuota struct {
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
	SourceURL string      `json:"source_url,omitempty"`
	FiveH     quotaWindow `json:"five_h"`
	Weekly    quotaWindow `json:"weekly"`
}

type ProviderReport struct {
	Provider string        `json:"provider"`
	Stats    ProviderStats `json:"stats"`
	Quota    providerQuota `json:"quota"`
}

// probeProviderQuota fetches and parses the MiniMax token-plan quota for a
// provider. Providers without a quota API (opencode-go) report an error.
func ProbeProviderQuota(ctx context.Context, p Provider) ProviderReport {
	report := ProviderReport{Provider: p.Name, Stats: p.Stats, Quota: providerQuota{}}
	if p.Family == "opencode-go" {
		report.Quota.OK = false
		report.Quota.Error = "no public quota API (GoUsageLimitError surfaces on request)"
		return report
	}
	data, source, err := fetchQuotaRemains(ctx, p.Key)
	report.Quota.SourceURL = source
	if err != nil {
		report.Quota.OK = false
		report.Quota.Error = err.Error()
		return report
	}
	quota, err := ParseQuotaPayload(data, time.Now().Unix())
	if err != nil {
		report.Quota.OK = false
		report.Quota.Error = err.Error()
		return report
	}
	quota.OK = true
	report.Quota = quota
	return report
}

// parseQuotaPayload parses the token-plan remains payload into a
// providerQuota, extracting the "general" model_remains entry.
func ParseQuotaPayload(data []byte, nowUnix int64) (providerQuota, error) {
	var quota providerQuota
	var payload struct {
		ModelRemains []map[string]any `json:"model_remains"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return quota, fmt.Errorf("decode: %v", err)
	}
	var general map[string]any
	for _, mr := range payload.ModelRemains {
		if n, _ := mr["model_name"].(string); n == "general" {
			general = mr
			break
		}
	}
	if general == nil {
		return quota, fmt.Errorf(`no "general" model_remains entry`)
	}
	toInt := func(v any) *int64 {
		switch x := v.(type) {
		case float64:
			i := int64(x)
			return &i
		case int64:
			return &x
		case int:
			i := int64(x)
			return &i
		}
		return nil
	}
	toInt32 := func(v any) *int {
		if p := toInt(v); p != nil {
			i := int(*p)
			return &i
		}
		return nil
	}
	now := nowUnix
	fill := func(win *quotaWindow, pctKey, totalKey, remainKey, statusKey, endKey, startKey string) {
		if v, ok := general[pctKey].(float64); ok {
			i := int(v)
			win.PctRemaining = &i
		}
		if t := toInt(general[totalKey]); t != nil {
			win.TotalCount = t
		}
		if r := toInt(general[remainKey]); r != nil {
			win.RemainingCount = r
			if win.TotalCount != nil {
				c := *win.TotalCount - *win.RemainingCount
				win.ConsumedCount = &c
			}
		}
		win.Status = toInt32(general[statusKey])
		if e := toInt(general[endKey]); e != nil {
			s := (*e)/1000 - now
			if s < 0 {
				s = 0
			}
			win.ResetInS = &s
			endSec := (*e) / 1000
			win.WindowEndUnix = &endSec
		}
		if s := toInt(general[startKey]); s != nil {
			startSec := (*s) / 1000
			win.WindowStartUnix = &startSec
		}
	}
	fill(&quota.FiveH, "current_interval_remaining_percent",
		"current_interval_total_count", "current_interval_usage_count",
		"current_interval_status", "end_time", "start_time")
	fill(&quota.Weekly, "current_weekly_remaining_percent",
		"current_weekly_total_count", "current_weekly_usage_count",
		"current_weekly_status", "weekly_end_time", "weekly_start_time")
	return quota, nil
}

// fetchQuotaRemains GETs the token-plan remains endpoint, falling back to
// the CN mirror when the primary URL fails or rejects the key. Returns the
// raw body, the URL that answered, and any error.
func fetchQuotaRemains(ctx context.Context, apiKey string) ([]byte, string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	url := quotaRemainsURL
	resp, err := doQuotaGet(client, ctx, url, apiKey)
	if err != nil || (resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden)) {
		if resp != nil {
			resp.Body.Close()
		}
		url = quotaRemainsCNURL
		resp, err = doQuotaGet(client, ctx, url, apiKey)
	}
	if err != nil {
		return nil, url, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, url, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, url, err
	}
	return data, url, nil
}

func doQuotaGet(client *http.Client, ctx context.Context, url, apiKey string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}
