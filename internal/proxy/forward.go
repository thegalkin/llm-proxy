package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// upstreamProxy holds the optional upstream-routing proxy parsed at startup
// from the routing config (proxy.url) or the LLM_PROXY_HTTP_PROXY env var.
// Populated by LoadUpstreamProxy(); consumed by forward() via proxyURLForFamily().
var (
	upstreamProxyMu   sync.RWMutex
	upstreamProxyFor  map[string]*url.URL // family -> *url.URL; nil entry == direct
)

// --- proxy engine: forward to upstream, with optional two-key failover ---

// upstreamClient builds the HTTP client used for a single upstream attempt
// chain. ResponseHeaderTimeout bounds the time until the upstream sends
// response headers — the upstream has been observed to hang indefinitely on
// large requests, and without a bound the proxy would hang forever. The SSE
// body stream itself is unbounded; it is cut short only by the client context
// (r.Context()), which also cancels the upstream request when the client
// disconnects. A fresh transport per call keeps a wedged keep-alive
// connection from poisoning later requests.
//
// proxyURL is an optional upstream-routing proxy (e.g. mihomo on
// 127.0.0.1:7897); nil means dial the upstream directly.
func upstreamClient(timeoutS int, proxyURL *url.URL) (*http.Client, *http.Transport) {
	d := time.Duration(timeoutS) * time.Second
	if d <= 0 {
		d = upstreamHeaderTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = d
	// http.DefaultTransport.Proxy defaults to http.ProxyFromEnvironment
	// (not nil), so a freshly-cloned transport inherits it. Without a
	// nil-cleared Proxy, a "no proxy configured" code path still hits
	// HTTPS_PROXY/HTTP_PROXY/no_proxy env vars — bypassing the proxy
	// contract entirely. Reset to nil by default; opt back in explicitly.
	tr.Proxy = nil
	if proxyURL != nil {
		tr.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: tr}, tr
}

// clientGone reports whether the downstream client has disconnected, in which
// case there is no point trying further keys or writing a response.
func clientGone(r *http.Request) bool {
	return r.Context().Err() != nil
}

// forward sends body to upstream.URL using upstream's auth. For type=minimax
// it does the two-key failover dance from the legacy proxy. For type=
// opencode-go it injects the Authorization header from the OPENCODE_GO_TOKEN
// env var (optional). For type=passthrough it does simple POST with no auth.
func forward(cfg *Config, w http.ResponseWriter, r *http.Request, body []byte, decision RoutingDecision, providers []Provider) {
	us := decision.Upstream
	switch us.Type {
	case "minimax":
		ForwardMinimax(w, r, body, us, providers)
	case "opencode-go":
		ForwardOpencodeGo(w, r, body, us, providers)
	case "opencode-zen":
		ForwardOpencodeZen(w, r, body, us, providers)
	case "ollama":
		ForwardOllama(w, r, body, us, providers)
	case "openrouter":
		ForwardOpenrouter(w, r, body, us, providers)
	case "passthrough":
		ForwardPassthrough(w, r, body, us)
	default:
		log.Printf("llm-proxy: unknown upstream type %q", us.Type)
		http.Error(w, "no upstream configured", http.StatusBadGateway)
	}
}

func ForwardMinimax(w http.ResponseWriter, r *http.Request, body []byte, us Upstream, providers []Provider) {
	httpClient, tr := upstreamClient(us.TimeoutS, proxyURLForFamily(us.Type))
	defer tr.CloseIdleConnections()
	upstreamURL := JoinTarget(us.BaseURL, us.URLPattern)
	ptrs := make([]*Provider, len(providers))
	for i := range providers {
		ptrs[i] = &providers[i]
	}
	for i, p := range buildAttemptOrder("minimax", ptrs) {
		log.Printf("minimax attempt %d/%d: provider=%s bytes=%d", i+1, len(providers), p.Name, len(body))

		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		req.Header.Set("anthropic-version", r.Header.Get("anthropic-version"))
		if v := r.Header.Get("anthropic-beta"); v != "" {
			req.Header.Set("anthropic-beta", v)
		}
		req.Header.Set("x-api-key", p.Key)
		req.Header.Set("Authorization", "Bearer "+p.Key)

		resp, err := httpClient.Do(req)
		if err != nil {
			if clientGone(r) {
				log.Printf("provider=%s aborted: client disconnected", p.Name)
				return
			}
			log.Printf("provider=%s transport error: %v", p.Name, err)
			p.Stats.observe(0)
			p.Stats.recordFailover()
			setCooldown(p, 0, "", nil)
			continue
		}
		p.Stats.observe(resp.StatusCode)

		ct := resp.Header.Get("Content-Type")
		isSSE := strings.Contains(strings.ToLower(ct), "text/event-stream")

		const peekCap = 4096
		peek := make([]byte, 0, peekCap)
		tee := io.TeeReader(resp.Body, &PeekBuf{Peek: &peek, Cap: peekCap})

		if isSSE {
			CopyHeaders(w.Header(), resp.Header)
			if !ContainsHeader(resp.Header, "X-Accel-Buffering") {
				w.Header().Set("X-Accel-Buffering", "no")
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(resp.StatusCode)
			fw := newFlushWriter(w)
			if len(peek) > 0 {
				if _, werr := fw.Write(peek); werr != nil {
					resp.Body.Close()
					log.Printf("provider=%s write err: %v", p.Name, werr)
					return
				}
			}
			streamErr := StreamSSE(fw, tee)
			resp.Body.Close()
			if streamErr != nil {
				log.Printf("provider=%s stream err: %v", p.Name, streamErr)
			}
			return
		}

		n, _ := io.ReadFull(tee, make([]byte, peekCap))
		firstChunk := append([]byte(nil), peek[:n]...)

		if IsRateLimitError(resp.StatusCode, firstChunk) {
			log.Printf("provider=%s returned 429 + rate_limit_error, retrying next", p.Name)
			p.Stats.recordFailover()
			setCooldown(p, resp.StatusCode, resp.Header.Get("Retry-After"), firstChunk)
			resp.Body.Close()
			continue
		}

		markKeySuccess("minimax", p)
		CopyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if len(firstChunk) > 0 {
			w.Write(firstChunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		if n < peekCap {
			resp.Body.Close()
			log.Printf("provider=%s done (buffered %d bytes)", p.Name, n)
			return
		}

		log.Printf("provider=%s streaming rest", p.Name)
		_, copyErr := io.Copy(w, resp.Body)
		resp.Body.Close()
		if copyErr != nil {
			log.Printf("provider=%s copy err: %v", p.Name, copyErr)
		}
		return
	}

	log.Printf("all providers exhausted with rate_limit_error")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "60")
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"llm-proxy: all configured providers exhausted on rate limit"}}`))
}

// joinTarget builds the final upstream URL from a base URL and a path
// pattern, avoiding a duplicated version segment: if base already ends
// with ".../v1" and pattern starts with "/v1/...", the "/v1" is not
// repeated. Examples:
//
//	("https://opencode.ai/zen/go/v1", "/v1/chat/completions") -> https://opencode.ai/zen/go/v1/chat/completions
//	("https://opencode.ai/zen/go/v1", "/chat/completions")     -> https://opencode.ai/zen/go/v1/chat/completions
func JoinTarget(base, pattern string) string {
	base = strings.TrimRight(base, "/")
	pattern = strings.TrimLeft(pattern, "/")
	if pattern == "" {
		return base
	}
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		first := pattern[:i]
		if strings.HasSuffix(base, "/"+first) {
			pattern = pattern[i+1:]
		}
	}
	return base + "/" + pattern
}

// SanitizeEmptyAssistantMessages drops assistant messages whose content
// array is empty. The opencode-go upstream rejects requests containing such
// messages with 400 ({"model":"<name>"}), even though the conversation is
// otherwise valid. Empty assistant turns carry no information, so dropping
// them is safe.
func SanitizeEmptyAssistantMessages(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	msgs, ok := m["messages"].([]any)
	if !ok {
		return body
	}
	changed := false
	filtered := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			filtered = append(filtered, raw)
			continue
		}
		if role, _ := msg["role"].(string); role == "assistant" {
			if content, ok := msg["content"].([]any); ok && len(content) == 0 {
				changed = true
				continue
			}
		}
		filtered = append(filtered, raw)
	}
	if !changed {
		return body
	}
	m["messages"] = filtered
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func ForwardOpencodeGo(w http.ResponseWriter, r *http.Request, body []byte, us Upstream, providers []Provider) {
	body = SanitizeEmptyAssistantMessages(body)
	httpClient, tr := upstreamClient(us.TimeoutS, proxyURLForFamily(us.Type))
	defer tr.CloseIdleConnections()
	target := JoinTarget(us.BaseURL, us.URLPattern)
	var keys []*Provider
	for i := range providers {
		if providers[i].Family == "opencode-go" {
			keys = append(keys, &providers[i])
		}
	}
	if len(keys) == 0 {
		log.Printf("opencode-go: no OPENCODE_GO_KEY_N providers configured")
		http.Error(w, "opencode-go: no keys configured", http.StatusBadGateway)
		return
	}
	sawTimeout := false
	// 400 Bad Request is treated as failover: the upstream may temporarily
	// reject a model per-key (e.g. "model not available on this key" with a
	// body like {"model":"<name>"}), while another key still serves it.
	// The first 400 body is kept so the client gets a truthful error when
	// every key rejects the request with 400.
	badRequestCount := 0
	var firstBadRequestBody []byte
	for i, p := range buildAttemptOrder("opencode-go", keys) {
		log.Printf("opencode-go attempt %d/%d: provider=%s target=%s bytes=%d", i+1, len(keys), p.Name, target, len(body))
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.Key)
		if strings.HasSuffix(target, "/messages") {
			req.Header.Set("x-api-key", p.Key)
			req.Header.Set("anthropic-version", "2023-06-01")
			if v := r.Header.Get("anthropic-beta"); v != "" {
				req.Header.Set("anthropic-beta", v)
			}
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			if clientGone(r) {
				log.Printf("opencode-go provider=%s aborted: client disconnected", p.Name)
				return
			}
			sawTimeout = true
			log.Printf("opencode-go provider=%s transport error: %v", p.Name, err)
			p.Stats.observe(0)
			p.Stats.recordFailover()
			setCooldown(p, 0, "", nil)
			continue
		}
		p.Stats.observe(resp.StatusCode)

		ct := resp.Header.Get("Content-Type")
		isSSE := strings.Contains(strings.ToLower(ct), "text/event-stream")

		const peekCap = 4096
		peek := make([]byte, 0, peekCap)
		tee := io.TeeReader(resp.Body, &PeekBuf{Peek: &peek, Cap: peekCap})

		if isSSE {
			markKeySuccess("opencode-go", p)
			CopyHeaders(w.Header(), resp.Header)
			if !ContainsHeader(resp.Header, "X-Accel-Buffering") {
				w.Header().Set("X-Accel-Buffering", "no")
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(resp.StatusCode)
			fw := newFlushWriter(w)
			if len(peek) > 0 {
				if _, werr := fw.Write(peek); werr != nil {
					resp.Body.Close()
					log.Printf("opencode-go provider=%s write err: %v", p.Name, werr)
					return
				}
			}
			streamErr := StreamSSE(fw, tee)
			resp.Body.Close()
			if streamErr != nil {
				log.Printf("opencode-go provider=%s stream err: %v", p.Name, streamErr)
			}
			if strings.HasSuffix(target, "/chat/completions") {
				// OpenAI-shape clients (DeepSeek chat-completions adapters)
				// require the terminating `data: [DONE]` event; the opencode-go
				// upstream ends its SSE body without one, so append it.
				// Anthropic-shape clients (/v1/messages) must not see it.
				fw.Write([]byte("data: [DONE]\n\n"))
			}
			return
		}

		n, _ := io.ReadFull(tee, make([]byte, peekCap))
		firstChunk := append([]byte(nil), peek[:n]...)

		if IsModelShapeError(resp.StatusCode, firstChunk) {
			// Model is not served on this upstream — rotating to another key
			// with the same model is pointless. Surface the upstream error
			// verbatim so the client sees the real cause instead of a
			// misleading "all keys exhausted" 429.
			log.Printf("opencode-go provider=%s returned %d (model shape), surfacing to client: %s", p.Name, resp.StatusCode, firstChunk)
			CopyHeaders(w.Header(), resp.Header)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			if len(firstChunk) > 0 {
				w.Write(firstChunk)
			} else {
				w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"llm-proxy: model not supported on this upstream"}}`))
			}
			resp.Body.Close()
			return
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusBadRequest {
			log.Printf("opencode-go provider=%s returned %d, retrying next: %s", p.Name, resp.StatusCode, firstChunk)
			p.Stats.recordFailover()
			setCooldown(p, resp.StatusCode, resp.Header.Get("Retry-After"), firstChunk)
			if resp.StatusCode == http.StatusBadRequest {
				badRequestCount++
				if firstBadRequestBody == nil {
					firstBadRequestBody = append([]byte(nil), firstChunk...)
				}
			}
			resp.Body.Close()
			continue
		}

		markKeySuccess("opencode-go", p)
		CopyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if len(firstChunk) > 0 {
			w.Write(firstChunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		if n < peekCap {
			resp.Body.Close()
			log.Printf("opencode-go provider=%s done (buffered %d bytes)", p.Name, n)
			return
		}

		log.Printf("opencode-go provider=%s streaming rest", p.Name)
		_, copyErr := io.Copy(w, resp.Body)
		resp.Body.Close()
		if copyErr != nil {
			log.Printf("opencode-go provider=%s copy err: %v", p.Name, copyErr)
		}
		return
	}

	if sawTimeout {
		log.Printf("opencode-go: all keys failed on timeout/transport errors")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"llm-proxy: all opencode-go keys timed out"}}`))
		return
	}

	if badRequestCount == len(keys) {
		log.Printf("opencode-go: all keys rejected request with 400")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if len(firstBadRequestBody) > 0 {
			w.Write(firstBadRequestBody)
		} else {
			w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"llm-proxy: all opencode-go keys rejected the request (400)"}}`))
		}
		return
	}

	log.Printf("opencode-go: all keys exhausted (401/403/429/400)")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"llm-proxy: all opencode-go keys exhausted"}}`))
}

func ForwardOpencodeZen(w http.ResponseWriter, r *http.Request, body []byte, us Upstream, providers []Provider) {
	target := JoinTarget(us.BaseURL, us.URLPattern)
	var keys []*Provider
	for i := range providers {
		if providers[i].Family == "opencode-zen" {
			keys = append(keys, &providers[i])
		}
	}
	if len(keys) == 0 {
		log.Printf("opencode-zen: no OPENCODE_ZEN_KEY[_N] providers configured")
		http.Error(w, "opencode-zen: no keys configured", http.StatusBadGateway)
		return
	}
	sawTimeout := false
	badRequestCount := 0
	var firstBadRequestBody []byte
	for i, p := range buildAttemptOrder("opencode-zen", keys) {
		log.Printf("opencode-zen attempt %d/%d: provider=%s target=%s bytes=%d", i+1, len(keys), p.Name, target, len(body))
		httpClient, tr := upstreamClient(us.TimeoutS, proxyURLForFamily(us.Type))
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.Key)
		if strings.HasSuffix(target, "/messages") {
			req.Header.Set("x-api-key", p.Key)
			req.Header.Set("anthropic-version", "2023-06-01")
			if v := r.Header.Get("anthropic-beta"); v != "" {
				req.Header.Set("anthropic-beta", v)
			}
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			if clientGone(r) {
				log.Printf("opencode-zen provider=%s aborted: client disconnected", p.Name)
				tr.CloseIdleConnections()
				return
			}
			sawTimeout = true
			log.Printf("opencode-zen provider=%s transport error: %v", p.Name, err)
			p.Stats.observe(0)
			p.Stats.recordFailover()
			setCooldown(p, 0, "", nil)
			tr.CloseIdleConnections()
			continue
		}
		p.Stats.observe(resp.StatusCode)
		ct := resp.Header.Get("Content-Type")
		isSSE := strings.Contains(strings.ToLower(ct), "text/event-stream")
		const peekCap = 4096
		peek := make([]byte, 0, peekCap)
		tee := io.TeeReader(resp.Body, &PeekBuf{Peek: &peek, Cap: peekCap})
		if isSSE {
			markKeySuccess("opencode-zen", p)
			CopyHeaders(w.Header(), resp.Header)
			if !ContainsHeader(resp.Header, "X-Accel-Buffering") {
				w.Header().Set("X-Accel-Buffering", "no")
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(resp.StatusCode)
			fw := newFlushWriter(w)
			if len(peek) > 0 {
				if _, werr := fw.Write(peek); werr != nil {
					resp.Body.Close()
					log.Printf("opencode-zen provider=%s write err: %v", p.Name, werr)
					tr.CloseIdleConnections()
					return
				}
			}
			streamErr := StreamSSE(fw, tee)
			resp.Body.Close()
			if streamErr != nil {
				log.Printf("opencode-zen provider=%s stream err: %v", p.Name, streamErr)
			}
			tr.CloseIdleConnections()
			return
		}
		n, _ := io.ReadFull(tee, make([]byte, peekCap))
		firstChunk := append([]byte(nil), peek[:n]...)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusBadRequest {
			log.Printf("opencode-zen provider=%s returned %d, retrying next: %s", p.Name, resp.StatusCode, firstChunk)
			p.Stats.recordFailover()
			setCooldown(p, resp.StatusCode, resp.Header.Get("Retry-After"), firstChunk)
			if resp.StatusCode == http.StatusBadRequest {
				badRequestCount++
				if firstBadRequestBody == nil {
					firstBadRequestBody = append([]byte(nil), firstChunk...)
				}
			}
			resp.Body.Close()
			tr.CloseIdleConnections()
			continue
		}
		markKeySuccess("opencode-zen", p)
		CopyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if len(firstChunk) > 0 {
			w.Write(firstChunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if n < peekCap {
			resp.Body.Close()
			log.Printf("opencode-zen provider=%s done (buffered %d bytes)", p.Name, n)
			tr.CloseIdleConnections()
			return
		}
		log.Printf("opencode-zen provider=%s streaming rest", p.Name)
		_, copyErr := io.Copy(w, resp.Body)
		resp.Body.Close()
		if copyErr != nil {
			log.Printf("opencode-zen provider=%s copy err: %v", p.Name, copyErr)
		}
		tr.CloseIdleConnections()
		return
	}
	if sawTimeout {
		log.Printf("opencode-zen: all keys failed on timeout/transport errors")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"llm-proxy: all opencode-zen keys timed out"}}`))
		return
	}
	if badRequestCount == len(keys) {
		log.Printf("opencode-zen: all keys rejected request with 400")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if len(firstBadRequestBody) > 0 {
			w.Write(firstBadRequestBody)
		} else {
			w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"llm-proxy: all opencode-zen keys rejected the request (400)"}}`))
		}
		return
	}
	log.Printf("opencode-zen: all keys exhausted (401/403/429/400)")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"llm-proxy: all opencode-zen keys exhausted"}}`))
}

func ForwardOllama(w http.ResponseWriter, r *http.Request, body []byte, us Upstream, providers []Provider) {
	var ollama *Provider
	for i := range providers {
		if providers[i].Family == "ollama" {
			ollama = &providers[i]
			break
		}
	}
	if ollama == nil {
		log.Printf("ollama: no OLLAMA_API_KEY configured")
		http.Error(w, "ollama: no key configured", http.StatusBadGateway)
		return
	}
	target := JoinTarget(us.BaseURL, us.URLPattern)
	log.Printf("ollama: provider=%s target=%s model=%s bytes=%d", ollama.Name, target, us.Model, len(body))

	httpClient, tr := upstreamClient(us.TimeoutS, proxyURLForFamily(us.Type))
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ollama.Key)

	resp, err := httpClient.Do(req)
	if err != nil {
		if clientGone(r) {
			log.Printf("ollama: aborted: client disconnected")
			return
		}
		log.Printf("ollama: transport error: %v", err)
		http.Error(w, "ollama: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ollama.Stats.observe(resp.StatusCode)

	ct := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(strings.ToLower(ct), "text/event-stream")
	const peekCap = 4096
	peek := make([]byte, 0, peekCap)
	tee := io.TeeReader(resp.Body, &PeekBuf{Peek: &peek, Cap: peekCap})

	if isSSE {
		CopyHeaders(w.Header(), resp.Header)
		if !ContainsHeader(resp.Header, "X-Accel-Buffering") {
			w.Header().Set("X-Accel-Buffering", "no")
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		fw := newFlushWriter(w)
		if len(peek) > 0 {
			fw.Write(peek)
		}
		StreamSSE(fw, tee)
		fw.Write([]byte("data: [DONE]\n\n"))
		return
	}

	CopyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil {
		log.Printf("ollama: copy err: %v", copyErr)
	}
}

// ForwardOpenrouter forwards to openrouter.ai's /v1/messages endpoint.
// Same multi-key rotation as ForwardOpencodeGo (sticky + cooldown), but the
// upstream serves an Anthropic-shaped protocol — no SanitizeEmptyAssistantMessages,
// no trailing "data: [DONE]\n\n". Both Authorization: Bearer and x-api-key
// are set so the proxy works regardless of which auth scheme openrouter
// accepts in a given code path.
func ForwardOpenrouter(w http.ResponseWriter, r *http.Request, body []byte, us Upstream, providers []Provider) {
	httpClient, tr := upstreamClient(us.TimeoutS, proxyURLForFamily(us.Type))
	defer tr.CloseIdleConnections()
	target := JoinTarget(us.BaseURL, us.URLPattern)
	var keys []*Provider
	for i := range providers {
		if providers[i].Family == "openrouter" {
			keys = append(keys, &providers[i])
		}
	}
	if len(keys) == 0 {
		log.Printf("openrouter: no OPENROUTER_KEY_N providers configured")
		http.Error(w, "openrouter: no keys configured", http.StatusBadGateway)
		return
	}
	sawTimeout := false
	badRequestCount := 0
	var firstBadRequestBody []byte
	for i, p := range buildAttemptOrder("openrouter", keys) {
		log.Printf("openrouter attempt %d/%d: provider=%s target=%s bytes=%d", i+1, len(keys), p.Name, target, len(body))
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.Key)
		req.Header.Set("x-api-key", p.Key)
		req.Header.Set("anthropic-version", "2023-06-01")
		if v := r.Header.Get("anthropic-beta"); v != "" {
			req.Header.Set("anthropic-beta", v)
		}
		// Optional attribution headers recommended by openrouter.ai — kept
		// short, no PII; helps openrouter route abuse reports back here
		// instead of banning the key.
		req.Header.Set("HTTP-Referer", "https://llm-proxy.local")
		req.Header.Set("X-Title", "llm-proxy")

		resp, err := httpClient.Do(req)
		if err != nil {
			if clientGone(r) {
				log.Printf("openrouter provider=%s aborted: client disconnected", p.Name)
				return
			}
			sawTimeout = true
			log.Printf("openrouter provider=%s transport error: %v", p.Name, err)
			p.Stats.observe(0)
			p.Stats.recordFailover()
			setCooldown(p, 0, "", nil)
			continue
		}
		p.Stats.observe(resp.StatusCode)

		ct := resp.Header.Get("Content-Type")
		isSSE := strings.Contains(strings.ToLower(ct), "text/event-stream")

		const peekCap = 4096
		peek := make([]byte, 0, peekCap)
		tee := io.TeeReader(resp.Body, &PeekBuf{Peek: &peek, Cap: peekCap})

		if isSSE {
			markKeySuccess("openrouter", p)
			CopyHeaders(w.Header(), resp.Header)
			if !ContainsHeader(resp.Header, "X-Accel-Buffering") {
				w.Header().Set("X-Accel-Buffering", "no")
			}
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(resp.StatusCode)
			fw := newFlushWriter(w)
			if len(peek) > 0 {
				if _, werr := fw.Write(peek); werr != nil {
					resp.Body.Close()
					log.Printf("openrouter provider=%s write err: %v", p.Name, werr)
					return
				}
			}
			streamErr := StreamSSE(fw, tee)
			resp.Body.Close()
			if streamErr != nil {
				log.Printf("openrouter provider=%s stream err: %v", p.Name, streamErr)
			}
			return
		}

		n, _ := io.ReadFull(tee, make([]byte, peekCap))
		firstChunk := append([]byte(nil), peek[:n]...)

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusBadRequest {
			log.Printf("openrouter provider=%s returned %d, retrying next: %s", p.Name, resp.StatusCode, firstChunk)
			p.Stats.recordFailover()
			setCooldown(p, resp.StatusCode, resp.Header.Get("Retry-After"), firstChunk)
			if resp.StatusCode == http.StatusBadRequest {
				badRequestCount++
				if firstBadRequestBody == nil {
					firstBadRequestBody = append([]byte(nil), firstChunk...)
				}
			}
			resp.Body.Close()
			continue
		}

		markKeySuccess("openrouter", p)
		CopyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if len(firstChunk) > 0 {
			w.Write(firstChunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		if n < peekCap {
			resp.Body.Close()
			log.Printf("openrouter provider=%s done (buffered %d bytes)", p.Name, n)
			return
		}

		log.Printf("openrouter provider=%s streaming rest", p.Name)
		_, copyErr := io.Copy(w, resp.Body)
		resp.Body.Close()
		if copyErr != nil {
			log.Printf("openrouter provider=%s copy err: %v", p.Name, copyErr)
		}
		return
	}

	if sawTimeout {
		log.Printf("openrouter: all keys failed on timeout/transport errors")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"llm-proxy: all openrouter keys timed out"}}`))
		return
	}

	if badRequestCount == len(keys) {
		log.Printf("openrouter: all keys rejected request with 400")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if len(firstBadRequestBody) > 0 {
			w.Write(firstBadRequestBody)
		} else {
			w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"llm-proxy: all openrouter keys rejected the request (400)"}}`))
		}
		return
	}

	log.Printf("openrouter: all keys exhausted (401/403/429/400)")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"llm-proxy: all openrouter keys exhausted"}}`))
}

func ForwardPassthrough(w http.ResponseWriter, r *http.Request, body []byte, us Upstream) {	httpClient, tr := upstreamClient(us.TimeoutS, proxyURLForFamily(us.Type))
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, us.BaseURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	if v := r.Header.Get("Authorization"); v != "" {
		req.Header.Set("Authorization", v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "passthrough: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	CopyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// UpstreamProxySpec describes one upstream-routing proxy entry from the
// routing config's proxy.for[] list. Only the families in For get dialed
// through URL; everything else dials directly.
type UpstreamProxySpec struct {
	URL string   `yaml:"url"`
	For []string `yaml:"for"`
}

// LoadUpstreamProxy parses proxy URLs and stores them in a per-family lookup.
// Routing precedence (first match wins per family):
//  1. env LLM_PROXY_HTTP_PROXY_FAMILY_<FAMILY>=<url>  (most specific)
//  2. env LLM_PROXY_HTTP_PROXY=<url>                   (fallback default)
//  3. config proxy.for[] entries (parsed earlier and passed in)
func LoadUpstreamProxy(envDefault string, specs []UpstreamProxySpec) error {
	families := []string{"minimax", "opencode-go", "opencode-zen", "ollama", "openrouter", "passthrough"}
	lookup := map[string]*url.URL{}

	add := func(family, raw string) error {
		if raw == "" {
			return nil
		}
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid proxy url for %s: %w", family, err)
		}
		if u.Scheme != "http" && u.Scheme != "socks5" {
			return fmt.Errorf("proxy scheme %q for %s not supported (use http:// or socks5://)", u.Scheme, family)
		}
		lookup[family] = u
		return nil
	}

	for _, s := range specs {
		var parsed *url.URL
		var firstErr error
		for _, family := range s.For {
			if _, exists := lookup[family]; exists {
				continue
			}
			if parsed == nil {
				u, err := url.Parse(s.URL)
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("invalid proxy url %q: %w", s.URL, err)
					}
					continue
				}
				if u.Scheme != "http" && u.Scheme != "socks5" {
					if firstErr == nil {
						firstErr = fmt.Errorf("proxy scheme %q not supported (use http:// or socks5://)", u.Scheme)
					}
					continue
				}
				parsed = u
			}
			lookup[family] = parsed
		}
		if firstErr != nil && parsed == nil {
			return firstErr
		}
	}

	for _, family := range families {
		if _, exists := lookup[family]; !exists {
			if err := add(family, envDefault); err != nil {
				return err
			}
		}
	}

	for _, family := range families {
		key := "LLM_PROXY_HTTP_PROXY_FAMILY_" + strings.ToUpper(strings.ReplaceAll(family, "-", "_"))
		if err := add(family, os.Getenv(key)); err != nil {
			return err
		}
	}

	upstreamProxyMu.Lock()
	upstreamProxyFor = lookup
	upstreamProxyMu.Unlock()
	for f, u := range lookup {
		if u != nil {
			log.Printf("llm-proxy: routing %s through %s://%s%s", f, u.Scheme, u.Host, u.Path)
		}
	}
	return nil
}

func proxyURLForFamily(family string) *url.URL {
	upstreamProxyMu.RLock()
	defer upstreamProxyMu.RUnlock()
	return upstreamProxyFor[family]
}

// ResetUpstreamProxyForTest clears the proxy lookup. Test-only.
func ResetUpstreamProxyForTest() {
	upstreamProxyMu.Lock()
	upstreamProxyFor = map[string]*url.URL{}
	upstreamProxyMu.Unlock()
}

// UpstreamProxyURLForFamily is the exported form of proxyURLForFamily, for tests.
func UpstreamProxyURLForFamily(family string) *url.URL { return proxyURLForFamily(family) }

// UpstreamClientForTest exposes upstreamClient for tests.
func UpstreamClientForTest(timeoutS int, family string) (*http.Client, *http.Transport) {
	return upstreamClient(timeoutS, proxyURLForFamily(family))
}
