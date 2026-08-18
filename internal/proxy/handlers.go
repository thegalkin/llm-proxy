package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// registerRoutes wires all HTTP endpoints onto mux.
func RegisterRoutes(mux *http.ServeMux, cfg *Config, providers []Provider) {
	mux.HandleFunc("/v1/messages", HandleMessages(cfg, providers))
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions(cfg, providers))
	mux.HandleFunc("/healthz", HandleHealthz(cfg, providers))
	mux.HandleFunc("/v1/models", HandleModels(cfg))
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
		decision := Decide(cfg, body, r.URL.Path, "")
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
		// "opencode-go/kimi-k3"). The routing decision parses that
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
			"mimo-v2.5-free",
			"nemotron-3-ultra-free", "north-mini-code-free", "qwen3.5-plus", "qwen3.6-plus",
		}
		for _, m := range zenModels {
			add("llm-proxy/opencode-zen/zen/" + m)
		}
		for _, id := range []string{
			"llm-proxy/GO/deepseek-v4-flash",
		} {
			add(id)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
	}
}
