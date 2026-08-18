package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
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
func upstreamClient(timeoutS int) (*http.Client, *http.Transport) {
	d := time.Duration(timeoutS) * time.Second
	if d <= 0 {
		d = upstreamHeaderTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = d
	return &http.Client{Transport: tr}, tr
}

// clientGone reports whether the downstream client has disconnected, in which
// case there is no point trying further keys or writing a response.
func clientGone(r *http.Request) bool {
	return r.Context().Err() != nil
}

// forward sends body to upstream.URL using upstream's auth. For type=
// opencode-go it injects the Authorization header from the OPENCODE_GO_TOKEN
// env var (optional). For type=passthrough it does simple POST with no auth.
func forward(cfg *Config, w http.ResponseWriter, r *http.Request, body []byte, decision RoutingDecision, providers []Provider) {
	us := decision.Upstream
	switch us.Type {
	case "opencode-go":
		ForwardOpencodeGo(w, r, body, us, providers)
	case "opencode-zen":
		ForwardOpencodeZen(w, r, body, us, providers)
	case "passthrough":
		ForwardPassthrough(w, r, body, us)
	default:
		log.Printf("llm-proxy: unknown upstream type %q", us.Type)
		http.Error(w, "no upstream configured", http.StatusBadGateway)
	}
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
	httpClient, tr := upstreamClient(us.TimeoutS)
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
	for i, p := range keys {
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
					log.Printf("opencode-go provider=%s write err: %v", p.Name, werr)
					return
				}
			}
			streamErr := StreamSSE(fw, tee)
			resp.Body.Close()
			if streamErr != nil {
				log.Printf("opencode-go provider=%s stream err: %v", p.Name, streamErr)
			}
			return
		}

		n, _ := io.ReadFull(tee, make([]byte, peekCap))
		firstChunk := append([]byte(nil), peek[:n]...)

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusBadRequest {
			log.Printf("opencode-go provider=%s returned %d, retrying next: %s", p.Name, resp.StatusCode, firstChunk)
			p.Stats.recordFailover()
			if resp.StatusCode == http.StatusBadRequest {
				badRequestCount++
				if firstBadRequestBody == nil {
					firstBadRequestBody = append([]byte(nil), firstChunk...)
				}
			}
			resp.Body.Close()
			continue
		}

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
	var zen *Provider
	for i := range providers {
		if providers[i].Family == "opencode-zen" {
			zen = &providers[i]
			break
		}
	}
	if zen == nil {
		log.Printf("opencode-zen: no OPENCODE_ZEN_KEY configured")
		http.Error(w, "opencode-zen: no key configured", http.StatusBadGateway)
		return
	}
	target := JoinTarget(us.BaseURL, us.URLPattern)
	log.Printf("opencode-zen: provider=%s target=%s model=%s bytes=%d", zen.Name, target, us.Model, len(body))

	httpClient, tr := upstreamClient(us.TimeoutS)
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+zen.Key)
	if v := r.Header.Get("anthropic-version"); v != "" {
		req.Header.Set("anthropic-version", v)
	}
	if v := r.Header.Get("anthropic-beta"); v != "" {
		req.Header.Set("anthropic-beta", v)
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		req.Header.Set("x-api-key", v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if clientGone(r) {
			log.Printf("opencode-zen: aborted: client disconnected")
			return
		}
		log.Printf("opencode-zen: transport error: %v", err)
		http.Error(w, "opencode-zen: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	zen.Stats.observe(resp.StatusCode)

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
		return
	}

	CopyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil {
		log.Printf("opencode-zen: copy err: %v", copyErr)
	}
}

func ForwardPassthrough(w http.ResponseWriter, r *http.Request, body []byte, us Upstream) {
	httpClient, tr := upstreamClient(us.TimeoutS)
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
