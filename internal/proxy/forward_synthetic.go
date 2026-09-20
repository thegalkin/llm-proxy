package proxy

// Role-routed dispatcher.
//
// See internal/proxy/synthetic for the per-role ladders and the data
// shape returned by Resolve(). ForwardSynthetic walks the ladder,
// attempting each (family, model-id) target: within a target it sweeps
// the family's keys (one HTTP round-trip per key, cooldown-aware) and
// streams the first 2xx to the downstream client. Transient upstream
// failures (400/401/403/404/429, transport errors, timeouts) cool the
// failing key and advance — to the next key, or the next entry once the
// family's keys are exhausted. A client disconnect propagates immediately.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-proxy/internal/proxy/synthetic"
)

// syntheticGoSession is the fallback session id for clients that do not
// send x-opencode-session: stable for the process lifetime so OpenCode Go
// keeps prompt-cache affinity instead of seeing a new session each request.
var syntheticGoSession = "llm-proxy-" + strconv.FormatInt(time.Now().UnixNano(), 36)

// ForwardSynthetic walks the role ladder and writes the first
// successful response back to the downstream client. See the package
// doc on forward_synthetic.go for the per-attempt contract.
func ForwardSynthetic(w http.ResponseWriter, r *http.Request, body []byte, model string, providers []Provider) {
	role := syntheticRole(model)
	ladder := synthetic.Cached(role)
	if len(ladder) == 0 {
		log.Printf("synthetic: unknown role %q", role)
		http.Error(w, "llm-proxy: unknown synthetic role", http.StatusBadRequest)
		return
	}

	for idx, t := range ladder {
		if r.Context().Err() != nil {
			return
		}

		bodyCopy := rewriteBodyModel(cloneBody(body), t.Model)

		httpClient, tr := upstreamClient(120, proxyURLForFamily(t.Family))

		if clientGone(r) {
			tr.CloseIdleConnections()
			return
		}

		// One dial. If upstream returns 2xx we stream to w; otherwise
		// drain body and continue to the next ladder entry.
		ok := syntheticAttempt(httpClient, tr, w, r, bodyCopy, t, providers, role)

		// Always close idle keep-alive before next ladder entry —
		// keeps a wedged upstream from poisoning later attempts.
		tr.CloseIdleConnections()

		if ok {
			log.Printf("synthetic[%s] attempt %d/%d %s:%s delivered %s",
				role, idx+1, len(ladder), t.Family, t.Model, t.Reason)
			return
		}
	}

	log.Printf("synthetic[%s]: ladder exhausted (%d entries)", role, len(ladder))
	http.Error(w, "llm-proxy: synthetic ladder exhausted", http.StatusBadGateway)
}

// failureDetail renders a short, single-line excerpt of an upstream failure
// body, so a non-2xx attempt is diagnosable from the journal alone. The whole
// body is deliberately not logged: it can be large, and the already-drained
// 8 KiB chunk carries everything setCooldown needs.
func failureDetail(chunk []byte) string {
	if len(chunk) == 0 {
		return ""
	}
	s := strings.Join(strings.Fields(string(chunk)), " ")
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return ", body=" + strconv.Quote(s)
}

// syntheticAttempt attempts one ladder entry, sweeping the entry's whole
// provider keyset (cooldown-aware, via buildAttemptOrder) until a key
// returns 2xx. On 2xx it copies headers + body to w and returns true. A
// failing key is cooled and the next tried; w is only touched after a 2xx,
// so a miss advances without side effects and false means every key failed.
//
// The commit point is the WriteHeader call inside this function.
// Once we have written a status to w, we MUST write a body (or close
// the connection); Go's net/http enforces this for non-1xx statuses.
func syntheticAttempt(httpClient *http.Client, tr *http.Transport, w http.ResponseWriter, r *http.Request, body []byte, t synthetic.Target, providers []Provider, role string) bool {
	var keys []*Provider
	for i := range providers {
		if providers[i].Family == t.Family {
			keys = append(keys, &providers[i])
		}
	}
	if len(keys) == 0 {
		log.Printf("synthetic[%s] attempt %s:%s no-keys",
			role, t.Family, t.Model)
		return false
	}
	// Sweep the whole family keyset per ladder entry — same as the direct
	// forwarders. buildAttemptOrder leads with the last-good / non-cooling
	// key and defers cooling ones, so a failure cools that key immediately
	// and the next key tried (here, and on later entries/requests) is fresh.
	for _, p := range buildAttemptOrder(t.Family, keys) {
		url, headers, convertedBody := buildSyntheticRequest(r, body, t, p.Key)
		sendBody := body
		if convertedBody != nil {
			sendBody = convertedBody
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(sendBody))
		if err != nil {
			log.Printf("synthetic[%s] %s:%s build-err: %v", role, t.Family, t.Model, err)
			return false
		}
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			log.Printf("synthetic[%s] %s:%s key=%s transport-err: %v",
				role, t.Family, t.Model, p.Name, err)
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Drain — bounded by 8 KiB to keep failures small; keep the
			// bytes so a "Resets in N" hint can size the cooldown window.
			chunk, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			_ = resp.Body.Close()
			setCooldown(p, resp.StatusCode, resp.Header.Get("Retry-After"), chunk)
			log.Printf("synthetic[%s] attempt %s:%s key=%s status=%d, next key%s",
				role, t.Family, t.Model, p.Name, resp.StatusCode, failureDetail(chunk))
			continue
		}

		// Commit. Copy upstream headers and stream body.
		markKeySuccess(t.Family, p)
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("X-LLM-Proxy-Synthetic-Role", role)
		w.Header().Set("X-LLM-Proxy-Synthetic-Step", t.Family+":"+t.Model)
		w.WriteHeader(resp.StatusCode)

		out := io.Writer(w)
		if isSSE(resp.Header) || isHeadlessStreamSafe(resp) {
			out = newFlushWriter(w)
		}
		_, _ = io.Copy(out, resp.Body)
		_ = resp.Body.Close()
		return true
	}
	return false
}

func buildSyntheticRequest(r *http.Request, body []byte, t synthetic.Target, apiKey string) (string, http.Header, []byte) {
	var baseURL, urlPattern string
	var convertedBody []byte
	switch t.Family {
	case synthetic.FamOpencodeGo:
		baseURL, urlPattern = OpencodeGoBaseURL, "/chat/completions"
	case synthetic.FamOpencodeZen:
		baseURL, urlPattern = opencodeZenBaseURL, "/chat/completions"
	case synthetic.FamMinimax:
		baseURL, urlPattern = DefaultMinimaxURL, "/v1/messages"
		convertedBody = openAIToAnthropic(body)
	case synthetic.FamOpenrouter:
		if strings.HasSuffix(r.URL.Path, "/messages") {
			baseURL, urlPattern = OpenrouterBaseURL, "/v1/messages"
		} else {
			baseURL, urlPattern = OpenrouterBaseURL, "/chat/completions"
		}
	case synthetic.FamOllama:
		baseURL, urlPattern = ollamaCloudBaseURL, "/chat/completions"
	default:
		baseURL, urlPattern = opencodeZenBaseURL, "/chat/completions"
	}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Authorization", "Bearer "+apiKey)
	if t.Family == synthetic.FamOpencodeGo {
		// OpenCode Go requires a stable per-conversation session id for
		// routing / prompt-cache; forward the client's when present.
		sid := r.Header.Get("x-opencode-session")
		if sid == "" {
			sid = syntheticGoSession
		}
		hdr.Set("x-opencode-session", sid)
	}
	if urlPattern == "/v1/messages" {
		hdr.Set("x-api-key", apiKey)
		hdr.Set("anthropic-version", "2023-06-01")
		if v := r.Header.Get("anthropic-beta"); v != "" {
			hdr.Set("anthropic-beta", v)
		}
	}
	return JoinTarget(baseURL, urlPattern), hdr, convertedBody
}

// openAIToAnthropic converts the OpenAI-shape chat-completions body
// into Anthropic-shape /v1/messages body. Used by the synthetic
// forwarder when targeting minimax's subscription tier (which only
// accepts Anthropic-shape).
//
// Input:  {model: "...", messages: [{role: "system"|"user"|"assistant",
//          content: "string"|[{type:"text",text:"..."}|{type:"image_..."}]}],
//          max_tokens: N, stream: bool, ...passthrough-extra}
// Output: {model: "...", system: "string"|null,
//          messages: [{role: "user"|"assistant",
//          content: "string"|[{type:"text",text:"..."}|image blocks]}]}
//          max_tokens: N, stream: bool, ...}
//
// We deliberately don't round-trip tools, tool_choice, response_format,
// or other OpenAI-only fields — the minimax tier doesn't support
// them today, and we'd rather advance on 400 than crash the forwarder.
func openAIToAnthropic(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}

	out := map[string]any{}
	if v, ok := m["model"].(string); ok {
		out["model"] = v
	}
	if v, ok := m["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := m["stream"]; ok {
		out["stream"] = v
	}
	// temperature / top_p / stop pass through unchanged when set.
	for _, k := range []string{"temperature", "top_p", "stop"} {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}

	msgsAny, _ := m["messages"].([]any)
	var msgs []any
	var sysText string
	for _, raw := range msgsAny {
		mm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" {
			if s, ok := mm["content"].(string); ok {
				if sysText != "" {
					sysText += "\n\n"
				}
				sysText += s
			} else if arr, ok := mm["content"].([]any); ok {
				for _, blk := range arr {
					if bm, ok := blk.(map[string]any); ok {
						if t, _ := bm["type"].(string); t == "text" {
							if s, ok := bm["text"].(string); ok {
								if sysText != "" {
									sysText += "\n\n"
								}
								sysText += s
							}
						}
					}
				}
			}
			continue
		}
		var block []any
		switch c := mm["content"].(type) {
		case string:
			block = []any{map[string]any{"type": "text", "text": c}}
		case []any:
			for _, raw := range c {
				if bm, ok := raw.(map[string]any); ok {
					if t, _ := bm["type"].(string); t == "" || t == "text" {
						if s, ok := bm["text"].(string); ok {
							block = append(block, map[string]any{"type": "text", "text": s})
						}
					} else {
						// Forward non-text blocks (image, tool_use, etc.)
						// verbatim. The minimax tier may not accept all
						// of them; that's an upstream concern, not ours.
						block = append(block, bm)
					}
				}
			}
		}
		if len(block) == 0 {
			continue
		}
		// Anthropic expects alternating user/assistant; merge
		// adjacent user blocks into one (rare but possible).
		if len(msgs) > 0 {
			prev, _ := msgs[len(msgs)-1].(map[string]any)
			if prevRole, _ := prev["role"].(string); prevRole == role {
				// Append blocks to previous. Keep role.
				prevBlocks, _ := prev["content"].([]any)
				prev["content"] = append(prevBlocks, block...)
				continue
			}
		}
		msgs = append(msgs, map[string]any{"role": role, "content": block})
	}
	if sysText != "" {
		out["system"] = sysText
	}
	if len(msgs) > 0 {
		out["messages"] = msgs
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return body
	}
	return encoded
}

func syntheticRole(model string) string {
	role := strings.TrimPrefix(model, "llm-proxy/synthetic/")
	role = strings.TrimPrefix(role, "synthetic/")
	return role
}

func cloneBody(b []byte) []byte { return append([]byte(nil), b...) }

func rewriteBodyModel(body []byte, newModel string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = newModel
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func isSSE(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

// isHeadlessStreamSafe reports whether the upstream uses streaming.
// omp's opencode-go pipeline expects chunked tokens; we always flush.
func isHeadlessStreamSafe(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if resp.ContentLength >= 0 {
		return false
	}
	if te := strings.ToLower(resp.Header.Get("Transfer-Encoding")); te != "" && te != "identity" {
		return true
	}
	if resp.ProtoAtLeast(1, 1) {
		return true
	}
	return false
}

// unused-import guards (when this file compiles standalone for tests)
var _ = time.Second
var _ = strconv.Itoa
