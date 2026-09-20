// llm-proxy — multi-provider LLM reverse proxy with YAML routing table.
//
// Replaces minimax-proxy. Adds:
//   - YAML routing table at ~/.config/llm-proxy/config.yaml (hot-reloadable
//     via SIGHUP — not implemented yet, but the loader is easy to re-run).
//   - OpenAI/Anthropic-shape routing by either request host or body model
//     field. Each rule selects an upstream and (optionally) rewrites the
//     "model" field to a canonical name the upstream actually serves.
//   - Provider types:
//     minimax   → internal MiniMax LB with two-key failover (legacy)
//     opencode-go → opencode-go subscription (https://opencode.ai/api/v1)
//     passthrough → direct upstream URL (model rewrite optional)
//
// Listens on 127.0.0.1:8443 by default. Anthropic-shape path /v1/messages
// continues to work without any prefix. Provider/model field rewriting
// happens BEFORE the body is forwarded upstream.
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"llm-proxy/internal/proxy"
	"llm-proxy/internal/proxy/synthetic"
)

func main() {
	proxy.LoadDotEnv()

	providers, err := proxy.LoadProviders()
	if err != nil {
		log.Fatalf("llm-proxy: %v", err)
	}

	// Read the synthetic ladder override now rather than on the first
	// synthetic request, so the load outcome is in the journal from boot.
	synthetic.EnsureLoaded()

	cfg := proxy.LoadRoutingConfig()
	addr := cfg.ListenAddr
	if env := os.Getenv("LISTEN_ADDR"); env != "" {
		addr = env
	}

	if err := proxy.LoadUpstreamProxy(os.Getenv("LLM_PROXY_HTTP_PROXY"), cfg.ProxySpecs); err != nil {
		log.Fatalf("llm-proxy: %v", err)
	}

	// Background goroutine: fetch openrouter free-models every 10 minutes
	// so /v1/models always reflects the live catalog without manual
	// edits. Cancel via the ctx passed to ListenAndServe below; in this
	// minimal main() the goroutine runs for the lifetime of the process.
	refreshCtx, refreshCancel := context.WithCancel(context.Background())
	defer refreshCancel()
	proxy.StartOpenRouterCatalogRefresher(refreshCtx)

	mux := http.NewServeMux()
	proxy.RegisterRoutes(mux, &cfg, providers)

	log.Printf("llm-proxy ready with %d providers, %d routing rules", len(providers), len(cfg.Rules))
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
