package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-proxy/internal/proxy"
)

// --- upstream hang / timeout behavior ---
//
// Bug: the upstream (opencode.ai) intermittently hangs for minutes on large
// requests. The proxy used http.Client{Timeout: 0} and a Background context,
// so a hanging upstream hung the proxy forever, opencode retried with a
// growing body, and the session burned tokens without progress.
//
// Required behavior: a configurable response-header timeout, failover to the
// next key on timeout, a 504 to the client when all keys time out, and
// cancellation of the upstream request when the client disconnects.

func TestForwardOpencodeGoTimeoutFailover(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // drain body so the server watches for disconnects
		if r.Header.Get("Authorization") == "Bearer k1" {
			<-r.Context().Done() // key1's upstream hangs until the proxy gives up
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"},
		{Name: "opencode-go-2", Family: "opencode-go", Key: "k2"},
	}
	us := proxy.Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions", TimeoutS: 1}
	rec := httptest.NewRecorder()
	start := time.Now()
	proxy.ForwardOpencodeGo(rec, httptest.NewRequest(http.MethodPost, "/chat/completions", nil), []byte(`{"model":"x"}`), us, providers)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if elapsed > 10*time.Second {
		t.Errorf("forward took %v after key1 timeout, want fast failover", elapsed)
	}
	if got := providers[0].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[0].FailoverHits = %d, want 1", got)
	}
	if got := providers[1].Stats.Requests2xx; got != 1 {
		t.Errorf("providers[1].Requests2xx = %d, want 1", got)
	}
}

func TestForwardOpencodeGoAllTimeout(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // drain body so the server watches for disconnects
		<-r.Context().Done()
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"},
		{Name: "opencode-go-2", Family: "opencode-go", Key: "k2"},
	}
	us := proxy.Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions", TimeoutS: 1}
	rec := httptest.NewRecorder()
	start := time.Now()
	proxy.ForwardOpencodeGo(rec, httptest.NewRequest(http.MethodPost, "/chat/completions", nil), []byte(`{"model":"x"}`), us, providers)
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body: %s)", rec.Code, rec.Body.String())
	}
	if elapsed > 10*time.Second {
		t.Errorf("all-timeout path took %v, want ~2s", elapsed)
	}
	if got := providers[0].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[0].FailoverHits = %d, want 1", got)
	}
	if got := providers[1].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[1].FailoverHits = %d, want 1", got)
	}
}

func TestForwardMinimaxTimeoutFailover(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // drain body so the server watches for disconnects
		if r.Header.Get("x-api-key") == "k1" {
			<-r.Context().Done() // key1's upstream hangs until the proxy gives up
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{
		{Name: "minimax-coding-plan", Family: "minimax", Key: "k1"},
		{Name: "minimax", Family: "minimax", Key: "k2"},
	}
	us := proxy.Upstream{Type: "minimax", BaseURL: upstreamSrv.URL, TimeoutS: 1}
	rec := httptest.NewRecorder()
	start := time.Now()
	proxy.ForwardMinimax(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), []byte(`{"model":"x"}`), us, providers)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if elapsed > 10*time.Second {
		t.Errorf("forward took %v after key1 timeout, want fast failover", elapsed)
	}
	if got := providers[0].Stats.FailoverHits; got != 1 {
		t.Errorf("providers[0].FailoverHits = %d, want 1", got)
	}
	if got := providers[1].Stats.Requests2xx; got != 1 {
		t.Errorf("providers[1].Requests2xx = %d, want 1", got)
	}
}

func TestForwardOpencodeGoCancelsUpstreamOnClientDisconnect(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // drain body so the server watches for disconnects
		<-r.Context().Done()        // upstream request must be cancelled by the proxy
		close(upstreamCancelled)
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"}}
	us := proxy.Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions"}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		proxy.ForwardOpencodeGo(httptest.NewRecorder(), req, []byte(`{"model":"x"}`), us, providers)
		close(done)
	}()

	time.Sleep(300 * time.Millisecond) // let the upstream request get in flight
	cancel()

	select {
	case <-upstreamCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was not cancelled when the client disconnected")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not return after client disconnect")
	}
}

// --- model-rewrite capture bug ---
//
// Bug: a rule `to: opencode-go/*` (used to route GO/* models to the
// opencode-go family while preserving the model name) rewrote the model to
// the literal "*" instead of the captured suffix. The upstream then answered
// "Model * is not supported". The minimax family handled the star; the
// opencode-go family did not.

func TestDecideStarCaptureOpencodeGo(t *testing.T) {
	cfg, err := proxy.LoadRoutingConfigFromString(`
rules:
  - from: "GO/*"
    to: "opencode-go/*"
`)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	body := []byte(`{"model":"GO/deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	dec := proxy.Decide(&cfg, body, "/v1/messages", "minimax")

	if dec.Upstream.Type != "opencode-go" {
		t.Errorf("upstream type = %q, want opencode-go", dec.Upstream.Type)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(dec.RewrittenBody, &out); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	if out.Model != "deepseek-v4-flash" {
		t.Errorf("rewritten model = %q, want deepseek-v4-flash (body: %s)", out.Model, dec.RewrittenBody)
	}
}

func TestDecideStarCaptureOpencodeGoWithNamespace(t *testing.T) {
	cfg, err := proxy.LoadRoutingConfigFromString(`
rules:
  - from: "llm-proxy/GO/*"
    to: "opencode-go/*"
`)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	body := []byte(`{"model":"GO/deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	dec := proxy.Decide(&cfg, body, "/v1/messages", "minimax")

	var out struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(dec.RewrittenBody, &out); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	if out.Model != "deepseek-v4-flash" {
		t.Errorf("rewritten model = %q, want deepseek-v4-flash", out.Model)
	}
}

// The built-in default config must route the models opencode sends out of the
// box (no config file): GO/* → opencode-go with the prefix stripped,
// MiniMax-* → minimax, opencode-zen/zen/* → opencode-zen.

func TestDefaultConfigRoutesModels(t *testing.T) {
	cfg, err := proxy.LoadRoutingConfigFromString("")
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	if len(cfg.Rules) == 0 {
		t.Fatal("default config has no routing rules")
	}

	check := func(t *testing.T, model, wantType, wantModel string) {
		t.Helper()
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
		dec := proxy.Decide(&cfg, body, "/v1/messages", "minimax")
		if dec.Upstream.Type != wantType {
			t.Errorf("model %s: upstream type = %q, want %q", model, dec.Upstream.Type, wantType)
		}
		var out struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(dec.RewrittenBody, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Model != wantModel {
			t.Errorf("model %s: rewritten = %q, want %q", model, out.Model, wantModel)
		}
	}
	check(t, "GO/deepseek-v4-flash", "opencode-go", "deepseek-v4-flash")
	check(t, "GO/minimax-m3", "opencode-go", "minimax-m3")
	check(t, "MiniMax-M3", "minimax", "MiniMax-M3")
	// The zen namespace is a routing label only; the upstream serves the
	// bare model name.
	check(t, "opencode-zen/zen/deepseek-v4-flash", "opencode-zen", "deepseek-v4-flash")
}

// --- configurable timeout ---

func TestLoadRoutingConfigTimeout(t *testing.T) {
	cfg, err := proxy.LoadRoutingConfigFromString(`
default_timeout_s: 45
rules:
  - from: "GO/*"
    to: "opencode-go/*"
    timeout_s: 30
`)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.DefaultUS.TimeoutS != 45 {
		t.Errorf("DefaultUS.TimeoutS = %d, want 45", cfg.DefaultUS.TimeoutS)
	}
	// Custom rules sort before the built-ins (priority 0), so the first
	// GO/* rule is the one from the config.
	if len(cfg.Rules) == 0 || cfg.Rules[0].MatchExpr != "GO/*" || cfg.Rules[0].Upstream.TimeoutS != 30 {
		t.Errorf("first GO/* rule = %+v, want TimeoutS 30 (total rules: %d)",
			cfg.Rules[0], len(cfg.Rules))
	}
}

func TestDecideDefaultTimeoutApplied(t *testing.T) {
	cfg, err := proxy.LoadRoutingConfigFromString("default_timeout_s: 77")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	dec := proxy.Decide(&cfg, []byte(`{"model":"GO/deepseek-v4-flash"}`), "/v1/messages", "minimax")
	if dec.Upstream.TimeoutS != 77 {
		t.Errorf("decided upstream TimeoutS = %d, want 77", dec.Upstream.TimeoutS)
	}
}

func TestForwardOpencodeGoExplicitTimeoutZeroMeansDefault(t *testing.T) {
	// TimeoutS == 0 must still produce a working request (default applies).
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"}}
	us := proxy.Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/chat/completions"}
	rec := httptest.NewRecorder()
	proxy.ForwardOpencodeGo(rec, httptest.NewRequest(http.MethodPost, "/chat/completions", nil), []byte(`{"model":"x"}`), us, providers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestForwardOpencodeGoBodyAndHeadersPreserved(t *testing.T) {
	// Regression: the forwarded request must carry the original body and the
	// anthropic headers, and must be created with the client context.
	got := make(chan string, 1)
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		got <- r.Header.Get("anthropic-version") + "|" + string(buf)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamSrv.Close()

	providers := []proxy.Provider{{Name: "opencode-go-1", Family: "opencode-go", Key: "k1"}}
	us := proxy.Upstream{Type: "opencode-go", BaseURL: upstreamSrv.URL, URLPattern: "/v1/messages"}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	proxy.ForwardOpencodeGo(rec, req, []byte(`{"model":"x"}`), us, providers)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case v := <-got:
		if v != "2023-06-01|{\"model\":\"x\"}" {
			t.Errorf("upstream saw %q, want 2023-06-01|{\"model\":\"x\"}", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never saw the request")
	}
}
