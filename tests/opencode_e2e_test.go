package proxy_test

// End-to-end test that encodes the real opencode session flow (verified live
// via `opencode run` in tmux against the paid deepseek-v4-flash model):
//
//   turn 1: system + user text            -> SSE stream starts
//   turn 2: + tools, user asks for a tool -> SSE stream with a tool_use block
//   turn 3: + assistant tool_use + user tool_result -> SSE stream with text
//
// The proxy must: route GO/* to the opencode-go family, rewrite the model to
// the canonical name, forward the body (including tool_use/tool_result
// blocks) unchanged, and relay the SSE stream event-by-event.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-proxy/internal/proxy"
)

type upstreamObs struct {
	model       string
	nTools      int
	toolUse     int // tool_use blocks in messages
	toolResults int // tool_result blocks in messages
	auth        string
	apiKey      string
	body        string
}

type mockOpencodeUpstream struct {
	mu   sync.Mutex
	seen []upstreamObs
}

// serve implements the upstream: records what it saw and replies with an
// Anthropic-style SSE stream whose tool_use block mentions the tool the
// request asked for.
func (m *mockOpencodeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	obs := upstreamObs{
		model:  fmt.Sprintf("%v", body["model"]),
		auth:   r.Header.Get("Authorization"),
		apiKey: r.Header.Get("x-api-key"),
	}
	if tools, ok := body["tools"].([]any); ok {
		obs.nTools = len(tools)
	}
	for _, raw := range body["messages"].([]any) {
		msg, _ := raw.(map[string]any)
		content, _ := msg["content"].([]any)
		for _, c := range content {
			blk, _ := c.(map[string]any)
			switch blk["type"] {
			case "tool_use":
				obs.toolUse++
			case "tool_result":
				obs.toolResults++
			}
		}
	}
	raw, _ := json.Marshal(body)
	obs.body = string(raw)

	m.mu.Lock()
	m.seen = append(m.seen, obs)
	m.mu.Unlock()

	toolUse := obs.nTools > 0 && obs.toolUse == 0 // first tools turn answers with a tool call
	stream := sseStream(obs.model, toolUse)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for _, ev := range stream {
		w.Write([]byte(ev))
		w.(http.Flusher).Flush()
	}
}

func (m *mockOpencodeUpstream) observations() []upstreamObs {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]upstreamObs(nil), m.seen...)
}

// sseStream builds the same event sequence the real opencode.ai upstream
// emits: message_start, content_block_start (tool_use when requested),
// content_block_stop, message_delta, message_stop.
func sseStream(model string, withToolUse bool) []string {
	events := []string{
		fmt.Sprintf("event: message_start\ndata: %s\n\n",
			`{"type":"message_start","message":{"id":"msg_1","role":"assistant","content":[],"model":"`+model+`","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}
	if withToolUse {
		events = append(events,
			"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`+"\n\n",
			"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`+"\n\n",
		)
	} else {
		events = append(events,
			"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n",
			"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`+"\n\n",
			"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`+"\n\n",
		)
	}
	events = append(events,
		"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`+"\n\n",
		"event: message_stop\ndata: "+`{"type":"message_stop"}`+"\n\n",
	)
	return events
}

// TestOpencodeToolUseLoop replays the opencode request sequence through the
// real proxy mux and a mock upstream, asserting the tool_use round-trip.
func TestOpencodeToolUseLoop(t *testing.T) {
	mock := &mockOpencodeUpstream{}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(mock.serve))
	defer upstreamSrv.Close()

	cfg, err := proxy.LoadRoutingConfigFromString(fmt.Sprintf(`
rules:
  - from: "GO/*"
    to: "opencode-go/*"
    base_url: "%s"
    url_pattern: "/v1/messages"
`, upstreamSrv.URL))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	providers := []proxy.Provider{{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"}}
	mux := http.NewServeMux()
	proxy.RegisterRoutes(mux, &cfg, providers)
	proxySrv := httptest.NewServer(mux)
	defer proxySrv.Close()

	client := &http.Client{Timeout: 30 * time.Second}
	turn := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("turn request: %v", err)
		}
		defer resp.Body.Close()
		buf := new(strings.Builder)
		_, _ = io.Copy(buf, resp.Body)
		return resp.StatusCode, buf.String()
	}

	tools := `"tools":[{"name":"bash","description":"Run a shell command","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]`

	// Turn 1: plain user message, no tools.
	code, body := turn(`{"model":"GO/deepseek-v4-flash","max_tokens":1024,"stream":true,"system":[{"type":"text","text":"You are a helpful agent."}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if code != http.StatusOK {
		t.Fatalf("turn 1 status = %d, body: %s", code, body)
	}
	if !strings.Contains(body, `"type":"message_start"`) || !strings.Contains(body, `"type":"text_delta"`) {
		t.Errorf("turn 1: expected a text SSE stream, got: %.200s", body)
	}

	// Turn 2: tools attached; the model answers with a tool_use block.
	code, body = turn(`{"model":"GO/deepseek-v4-flash","max_tokens":1024,"stream":true,"system":[{"type":"text","text":"You are a helpful agent."}],` + tools + `,"messages":[{"role":"user","content":[{"type":"text","text":"List /tmp with the bash tool"}]}]}`)
	if code != http.StatusOK {
		t.Fatalf("turn 2 status = %d, body: %s", code, body)
	}
	if !strings.Contains(body, `"type":"tool_use"`) || !strings.Contains(body, `"name":"bash"`) {
		t.Errorf("turn 2: expected a tool_use SSE stream, got: %.300s", body)
	}

	// Turn 3: assistant tool_use + user tool_result are sent back; the model
	// answers with text. This is the loop that used to hang/fail.
	toolResult := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"total 4"},{"type":"text","text":"continue"}]}`
	assistant := `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"ls -la /tmp"}}]}`
	code, body = turn(`{"model":"GO/deepseek-v4-flash","max_tokens":1024,"stream":true,"system":[{"type":"text","text":"You are a helpful agent."}],` + tools + `,"messages":[{"role":"user","content":[{"type":"text","text":"List /tmp with the bash tool"}]},` + assistant + `,` + toolResult + `]}`)
	if code != http.StatusOK {
		t.Fatalf("turn 3 status = %d, body: %s", code, body)
	}
	if !strings.Contains(body, `"type":"text_delta"`) {
		t.Errorf("turn 3: expected a text SSE stream, got: %.300s", body)
	}

	seen := mock.observations()
	if len(seen) != 3 {
		t.Fatalf("upstream saw %d requests, want 3", len(seen))
	}
	for i, s := range seen {
		if s.model != "deepseek-v4-flash" {
			t.Errorf("turn %d: upstream model = %q, want deepseek-v4-flash (rewritten)", i+1, s.model)
		}
		if s.auth != "Bearer k1" || s.apiKey != "k1" {
			t.Errorf("turn %d: auth = %q / x-api-key = %q, want Bearer k1 / k1", i+1, s.auth, s.apiKey)
		}
	}
	if seen[0].nTools != 0 || seen[1].nTools != 1 || seen[2].nTools != 1 {
		t.Errorf("tools seen upstream: %d/%d/%d, want 0/1/1", seen[0].nTools, seen[1].nTools, seen[2].nTools)
	}
	if seen[2].toolUse != 1 || seen[2].toolResults != 1 {
		t.Errorf("turn 3 blocks upstream: tool_use=%d tool_result=%d, want 1/1 (body: %.300s)",
			seen[2].toolUse, seen[2].toolResults, seen[2].body)
	}
	if seen[1].toolUse != 0 {
		t.Errorf("turn 2: tool_use blocks in messages = %d, want 0", seen[1].toolUse)
	}
}

// TestOpencodeZenRoute verifies the zen family path: the Anthropic-shape
// request must reach the upstream at /v1/messages (not /v1/v1/messages — the
// base URL already ends with /v1) with the model rewritten to "zen/<name>".
func TestOpencodeZenRoute(t *testing.T) {
	var gotPath string
	var gotModel string
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range sseStream(body.Model, false) {
			w.Write([]byte(ev))
			w.(http.Flusher).Flush()
		}
	}))
	defer upstreamSrv.Close()

	cfg, err := proxy.LoadRoutingConfigFromString(fmt.Sprintf(`
rules:
  - from: "opencode-zen/zen/*"
    to: "opencode-zen zen/*"
    base_url: "%s"
    url_pattern: "/chat/completions"
`, upstreamSrv.URL))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	providers := []proxy.Provider{{Name: "opencode-zen", Family: "opencode-zen", Key: "zenkey"}}
	mux := http.NewServeMux()
	proxy.RegisterRoutes(mux, &cfg, providers)
	proxySrv := httptest.NewServer(mux)
	defer proxySrv.Close()

	body := `{"model":"opencode-zen/zen/big-pickle","max_tokens":50,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("zen request: %v", err)
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	_, _ = io.Copy(buf, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body: %.200s", resp.StatusCode, buf.String())
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages (no duplicate /v1)", gotPath)
	}
	if gotModel != "big-pickle" {
		t.Errorf("upstream model = %q, want big-pickle (bare name, no zen/ prefix)", gotModel)
	}
}

// TestOpencodeLargeContextNoHang replays the large (~1 MB) opencode-style
// request that the real upstream used to hang on. Through the proxy it must
// complete promptly with a streamed response.
func TestOpencodeLargeContextNoHang(t *testing.T) {
	mock := &mockOpencodeUpstream{}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(mock.serve))
	defer upstreamSrv.Close()

	cfg, err := proxy.LoadRoutingConfigFromString(fmt.Sprintf(`
rules:
  - from: "GO/*"
    to: "opencode-go/*"
    base_url: "%s"
    url_pattern: "/v1/messages"
`, upstreamSrv.URL))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	providers := []proxy.Provider{{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"}}
	mux := http.NewServeMux()
	proxy.RegisterRoutes(mux, &cfg, providers)
	proxySrv := httptest.NewServer(mux)
	defer proxySrv.Close()

	// Build the same shape as the real opencode session: hundreds of
	// tool_use/tool_result turns with text blocks, ~1 MB total.
	msgs := []string{`{"role":"user","content":[{"type":"text","text":"Start"}]}`}
	for i := 0; i < 3000; i++ {
		msgs = append(msgs,
			fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"step %d"},{"type":"tool_use","id":"toolu_%d","name":"bash","input":{"command":"echo %d"}}]}`, i, i, i),
			fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":"output %d"},{"type":"text","text":"continue %s"}]}`, i, i, strings.Repeat("z", 20)),
		)
	}
	body := `{"model":"GO/deepseek-v4-flash","max_tokens":8192,"stream":true,"system":[{"type":"text","text":"You are a helpful agent."}],` +
		`"tools":[{"name":"bash","description":"run shell","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],` +
		`"messages":[` + strings.Join(msgs, ",") + `]}`
	if len(body) < 900*1024 {
		t.Fatalf("test body too small: %d bytes, want >= ~900KB", len(body))
	}

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	start := time.Now()
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("large request: %v", err)
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	_, _ = io.Copy(buf, resp.Body)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body: %.300s", resp.StatusCode, buf.String())
	}
	if elapsed > 30*time.Second {
		t.Errorf("large request took %v, want prompt completion (no hang)", elapsed)
	}
	if !strings.Contains(buf.String(), `"type":"message_start"`) {
		t.Errorf("expected an SSE stream, got: %.200s", buf.String())
	}
	seen := mock.observations()
	if len(seen) != 1 || seen[0].toolUse != 3000 || seen[0].toolResults != 3000 {
		t.Errorf("upstream saw %d request(s), tool_use=%d tool_result=%d; want 1/3000/3000",
			len(seen), seen[0].toolUse, seen[0].toolResults)
	}
}
