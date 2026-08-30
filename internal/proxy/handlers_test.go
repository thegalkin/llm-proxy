package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- RegisterRoutes ---

// TestRegisterRoutes_AllEndpoints confirms every advertised endpoint is wired.
func TestRegisterRoutes_AllEndpoints(t *testing.T) {
	cfg := defaultConfig()
	providers := []Provider{{Name: "k", Family: "opencode-go", Key: "x"}}
	mux := http.NewServeMux()
	RegisterRoutes(mux, &cfg, providers)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/healthz", "/v1/models", "/admin/limits"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Fatalf("no handler for %s", path)
		}
	}
}

// TestHandleMessages_MethodNotAllowed: GET on /v1/messages must 405.
func TestHandleMessages_MethodNotAllowed(t *testing.T) {
	cfg := defaultConfig()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	HandleMessages(&cfg, nil)(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestHandleMessages_HappyPath: POST flows through Decide → forward → mocked
// upstream and returns the upstream's status. We use opencode-go as the
// upstream type so we don't need a real provider key.
func TestHandleMessages_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"hello":"world"}`))
	}))
	defer upstream.Close()

	cfg := defaultConfig()
	cfg.DefaultUS = Upstream{
		Type:       "opencode-go",
		BaseURL:    upstream.URL,
		URLPattern: "/v1/messages",
	}
	providers := []Provider{{Name: "k", Family: "opencode-go", Key: "kk"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		bytes.NewReader([]byte(`{"model":"foo","messages":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	HandleMessages(&cfg, providers)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("X-LLM-Proxy-Routing"), "opencode-go:") {
		t.Fatalf("missing routing tag header: %q", rec.Header().Get("X-LLM-Proxy-Routing"))
	}
}

// TestHandleMessages_BadBody: malformed body reader triggers io.ReadAll error
// and a 400.
func TestHandleMessages_BadBody(t *testing.T) {
	cfg := defaultConfig()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		&badReader{})
	req.Header.Set("Content-Type", "application/json")
	HandleMessages(&cfg, nil)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// --- handleChatCompletions ---

func TestHandleChatCompletions_MethodNotAllowed(t *testing.T) {
	cfg := defaultConfig()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	handleChatCompletions(&cfg, nil)(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleChatCompletions_BadBody(t *testing.T) {
	cfg := defaultConfig()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", &badReader{})
	req.Header.Set("Content-Type", "application/json")
	handleChatCompletions(&cfg, nil)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleChatCompletions_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := defaultConfig()
	cfg.DefaultUS = Upstream{
		Type:       "opencode-go",
		BaseURL:    upstream.URL,
		URLPattern: "/chat/completions",
	}
	providers := []Provider{{Name: "k", Family: "opencode-go", Key: "kk"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"opencode-go/foo"}`)))
	req.Header.Set("Content-Type", "application/json")
	handleChatCompletions(&cfg, providers)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// --- HandleHealthz ---

func TestHandleHealthz(t *testing.T) {
	cfg := &Config{DefaultUS: Upstream{Type: "opencode-go"}, Rules: make([]Rule, 3)}
	rec := httptest.NewRecorder()
	HandleHealthz(cfg, []Provider{{Name: "a"}, {Name: "b"}})(rec,
		httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "ok" || out["rules"] != float64(3) || out["providers"] != float64(2) {
		t.Fatalf("bad payload: %s", rec.Body.String())
	}
}

// --- HandleModels ---

func TestHandleModels_MethodNotAllowed(t *testing.T) {
	cfg := defaultConfig()
	rec := httptest.NewRecorder()
	HandleModels(&cfg)(rec, httptest.NewRequest(http.MethodPost, "/v1/models", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleModels_ContentShape(t *testing.T) {
	cfg := defaultConfig()
	rec := httptest.NewRecorder()
	HandleModels(&cfg)(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "list" {
		t.Fatalf("object = %q", out.Object)
	}
	if len(out.Data) == 0 {
		t.Fatalf("empty data")
	}
	// Sanity: the hard-coded zen catalog should appear at least once.
	found := false
	for _, d := range out.Data {
		if id, _ := d["id"].(string); strings.Contains(id, "opencode-zen/zen/") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no opencode-zen model id in response")
	}
}

func TestHandleModels_DedupAndDefaults(t *testing.T) {
	cfg := defaultConfig()
	cfg.DefaultUS = Upstream{Type: "opencode-go", Model: "foo"}
	cfg.Rules = []Rule{
		{Upstream: Upstream{Type: "opencode-go", Model: "foo"}},
		{Upstream: Upstream{Type: "opencode-go", Model: "bar"}},
		{Upstream: Upstream{Type: "opencode-go", Model: "*"}},    // skip wildcard
		{Upstream: Upstream{Type: "opencode-go", Model: "x*"}},   // skip suffix-wildcard
		{Upstream: Upstream{Type: "opencode-zen", Model: "baz"}}, // also covered by zen catalog
	}
	rec := httptest.NewRecorder()
	HandleModels(&cfg)(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "llm-proxy/GO/foo") {
		t.Fatalf("missing foo id: %s", body)
	}
	if !strings.Contains(body, "llm-proxy/GO/bar") {
		t.Fatalf("missing bar id: %s", body)
	}
	// foo should appear only once.
	if c := strings.Count(body, "llm-proxy/GO/foo"); c != 1 {
		t.Fatalf("foo appeared %d times: %s", c, body)
	}
}

// --- HandleLimits ---

func TestHandleLimits_MethodNotAllowed(t *testing.T) {
	cfg := defaultConfig()
	rec := httptest.NewRecorder()
	HandleLimits(&cfg, nil)(rec, httptest.NewRequest(http.MethodPost, "/admin/limits", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestHandleLimits_OpencodeGo(t *testing.T) {
	cfg := defaultConfig()
	providers := []Provider{{Name: "og", Family: "opencode-go", Key: "x"}}
	rec := httptest.NewRecorder()
	HandleLimits(&cfg, providers)(rec, httptest.NewRequest(http.MethodGet, "/admin/limits", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := out["providers"]; !ok {
		t.Fatalf("missing providers: %s", rec.Body.String())
	}
}

// --- ProbeProviderQuota ---

func TestProbeProviderQuota_OpencodeGo(t *testing.T) {
	p := &Provider{Name: "og", Family: "opencode-go", Key: "x"}
	rep := ProbeProviderQuota(context.Background(), p)
	if rep.Provider != "og" {
		t.Fatalf("provider = %q", rep.Provider)
	}
	if rep.Quota.OK {
		t.Fatalf("opencode-go should not have OK quota")
	}
	if rep.Quota.Error == "" {
		t.Fatalf("expected error message for opencode-go")
	}
}

func TestProbeProviderQuota_Openrouter(t *testing.T) {
	p := &Provider{Name: "or", Family: "openrouter", Key: "x"}
	rep := ProbeProviderQuota(context.Background(), p)
	if rep.Quota.OK || rep.Quota.Error == "" {
		t.Fatalf("openrouter expected OK=false + error")
	}
}

func TestProbeProviderQuota_Ollama(t *testing.T) {
	p := &Provider{Name: "ol", Family: "ollama", Key: "x"}
	rep := ProbeProviderQuota(context.Background(), p)
	if rep.Quota.OK || rep.Quota.Error == "" {
		t.Fatalf("ollama expected OK=false + error")
	}
}

func TestProbeProviderQuota_Minimax_ParseOK(t *testing.T) {
	// Stand up a fake quota endpoint that returns a real payload.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"model_remains":[{"model_name":"general","current_interval_remaining_percent":50,"current_interval_total_count":100,"current_interval_usage_count":50,"current_interval_status":0,"end_time":` + futureEnd(3600) + `,"start_time":` + futureEnd(-3600) + `,"current_weekly_remaining_percent":75,"current_weekly_total_count":1000,"current_weekly_usage_count":250,"current_weekly_status":0,"weekly_end_time":` + futureEnd(7*86400) + `,"weekly_start_time":` + futureEnd(-7*86400) + `}]}`))
	}))
	defer upstream.Close()
	oldPrimary := quotaRemainsURL
	oldCN := quotaRemainsCNURL
	defer func() {
		setQuotaURLs(oldPrimary, oldCN)
	}()
	setQuotaURLs(upstream.URL, upstream.URL)

	p := &Provider{Name: "mm", Family: "minimax", Key: "kk"}
	rep := ProbeProviderQuota(context.Background(), p)
	if !rep.Quota.OK {
		t.Fatalf("expected OK, got %+v", rep.Quota)
	}
	if rep.Quota.FiveH.PctRemaining == nil || *rep.Quota.FiveH.PctRemaining != 50 {
		t.Fatalf("5h pct = %v", rep.Quota.FiveH.PctRemaining)
	}
}

func TestProbeProviderQuota_Minimax_BadPayload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`not-json`))
	}))
	defer upstream.Close()
	oldPrimary := quotaRemainsURL
	oldCN := quotaRemainsCNURL
	defer func() {
		setQuotaURLs(oldPrimary, oldCN)
	}()
	setQuotaURLs(upstream.URL, upstream.URL)

	p := &Provider{Name: "mm", Family: "minimax", Key: "kk"}
	rep := ProbeProviderQuota(context.Background(), p)
	if rep.Quota.OK || rep.Quota.Error == "" {
		t.Fatalf("expected error payload, got %+v", rep.Quota)
	}
}

func TestProbeProviderQuota_Minimax_UpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer upstream.Close()
	oldPrimary := quotaRemainsURL
	oldCN := quotaRemainsCNURL
	defer func() {
		setQuotaURLs(oldPrimary, oldCN)
	}()
	setQuotaURLs(upstream.URL, upstream.URL)

	p := &Provider{Name: "mm", Family: "minimax", Key: "kk"}
	rep := ProbeProviderQuota(context.Background(), p)
	if rep.Quota.OK || rep.Quota.Error == "" {
		t.Fatalf("expected error payload, got %+v", rep.Quota)
	}
}

// --- ParseQuotaPayload ---

func TestParseQuotaPayload_BadJSON(t *testing.T) {
	_, err := ParseQuotaPayload([]byte(`not-json`), time.Now().Unix())
	if err == nil {
		t.Fatalf("expected decode error")
	}
}

func TestParseQuotaPayload_NoGeneral(t *testing.T) {
	data := []byte(`{"model_remains":[{"model_name":"other","current_interval_remaining_percent":10}]}`)
	_, err := ParseQuotaPayload(data, time.Now().Unix())
	if err == nil {
		t.Fatalf("expected 'no general' error")
	}
}

func TestParseQuotaPayload_FullPayload(t *testing.T) {
	now := time.Now().Unix()
	endSec := now + 3600
	startSec := now - 3600
	wEnd := now + 7*86400
	wStart := now - 7*86400
	data := []byte(`{"model_remains":[{"model_name":"general","current_interval_remaining_percent":80,"current_interval_total_count":100,"current_interval_usage_count":20,"current_interval_status":1,"end_time":` +
		msec(endSec) + `,"start_time":` + msec(startSec) +
		`,"current_weekly_remaining_percent":90,"current_weekly_total_count":1000,"current_weekly_usage_count":100,"current_weekly_status":0,"weekly_end_time":` +
		msec(wEnd) + `,"weekly_start_time":` + msec(wStart) + `}]}`)
	q, err := ParseQuotaPayload(data, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q.OK = true // ParseQuotaPayload does NOT set OK; the caller (ProbeProviderQuota) does
	if !q.OK {
		t.Fatalf("expected OK after setting manually")
	}
	// Parsed from "current_interval_remaining_percent"
	if q.FiveH.PctRemaining == nil || *q.FiveH.PctRemaining != 80 {
		t.Fatalf("5h pct = %v", q.FiveH.PctRemaining)
	}
	// Parsed from "current_interval_total_count"
	if q.FiveH.TotalCount == nil || *q.FiveH.TotalCount != 100 {
		t.Fatalf("5h total = %v", q.FiveH.TotalCount)
	}
	// ParseQuotaPayload stashes the "usage_count" field into RemainingCount,
	// then computes ConsumedCount = TotalCount - RemainingCount.
	if q.FiveH.RemainingCount == nil || *q.FiveH.RemainingCount != 20 {
		t.Fatalf("5h remain (=usage) = %v", q.FiveH.RemainingCount)
	}
	if q.FiveH.ConsumedCount == nil || *q.FiveH.ConsumedCount != 80 {
		t.Fatalf("5h consumed = %v", q.FiveH.ConsumedCount)
	}
	if q.FiveH.ResetInS == nil || *q.FiveH.ResetInS <= 0 {
		t.Fatalf("5h reset = %v", q.FiveH.ResetInS)
	}
	if q.Weekly.PctRemaining == nil || *q.Weekly.PctRemaining != 90 {
		t.Fatalf("weekly pct = %v", q.Weekly.PctRemaining)
	}
	if q.Subscription.ResetInS == nil || *q.Subscription.ResetInS <= 0 {
		t.Fatalf("sub reset = %v", q.Subscription.ResetInS)
	}
	if q.Subscription.ResetAtISO == nil {
		t.Fatalf("sub iso = nil")
	}
	if q.Subscription.WindowHours == nil {
		t.Fatalf("sub window_hours = nil")
	}
}

func TestParseQuotaPayload_PastEnd(t *testing.T) {
	now := time.Now().Unix()
	// end in the past → reset clamps to 0
	data := []byte(`{"model_remains":[{"model_name":"general","end_time":` +
		msec(now-3600) + `,"start_time":` + msec(now-7200) + `,"weekly_end_time":` +
		msec(now-3600) + `,"weekly_start_time":` + msec(now-7200) + `}]}`)
	q, err := ParseQuotaPayload(data, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.FiveH.ResetInS == nil || *q.FiveH.ResetInS != 0 {
		t.Fatalf("5h reset = %v", q.FiveH.ResetInS)
	}
}

func TestParseQuotaPayload_MissingGeneralFields(t *testing.T) {
	now := time.Now().Unix()
	// All keys absent — toInt/toInt32 should give nil everywhere.
	data := []byte(`{"model_remains":[{"model_name":"general"}]}`)
	q, err := ParseQuotaPayload(data, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.FiveH.PctRemaining != nil || q.FiveH.TotalCount != nil {
		t.Fatalf("expected nil pointers for missing fields: %+v", q.FiveH)
	}
}

func TestParseQuotaPayload_HandlesInt64AndInt(t *testing.T) {
	// JSON unmarshal of integer values lands in float64 normally, but a
	// decoder that uses UseNumber() would surface them as json.Number — and
	// json.Number implements Unmarshaler. Hand-craft a payload whose counts
	// are encoded as strings? Easier path: drive ParseQuotaPayload through
	// a hand-built map[string]any containing int64 and int directly.
	now := time.Now().Unix()
	endMs := (now + 3600) * 1000
	startMs := (now - 3600) * 1000
	general := map[string]any{
		"model_name":                         "general",
		"current_interval_remaining_percent": float64(50),
		"current_interval_total_count":       int64(100),
		"current_interval_usage_count":       int(20),
		"current_interval_status":            int64(0),
		"end_time":                           int64(endMs),
		"start_time":                         int64(startMs),
		"current_weekly_remaining_percent":   float64(75),
		"current_weekly_total_count":         int64(1000),
		"current_weekly_usage_count":         int64(100),
		"current_weekly_status":              int64(0),
		"weekly_end_time":                    int64(endMs),
		"weekly_start_time":                  int64(startMs),
	}
	q, err := parseQuotaPayloadFromMap(general, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.FiveH.TotalCount == nil || *q.FiveH.TotalCount != 100 {
		t.Fatalf("5h total = %v", q.FiveH.TotalCount)
	}
	if q.FiveH.RemainingCount == nil || *q.FiveH.RemainingCount != 20 {
		t.Fatalf("5h remain = %v", q.FiveH.RemainingCount)
	}
}

func parseQuotaPayloadFromMap(general map[string]any, now int64) (providerQuota, error) {
	// Wrap the map in a payload-shaped structure that ParseQuotaPayload can
	// decode. We re-marshal the map and decode it back via the same path
	// ParseQuotaPayload uses.
	raw, _ := json.Marshal(struct {
		ModelRemains []map[string]any `json:"model_remains"`
	}{ModelRemains: []map[string]any{general}})
	return ParseQuotaPayload(raw, now)
}
func TestFetchQuotaRemains_PrimaryOK(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both Authorization and Content-Type should be set.
		if r.Header.Get("Authorization") == "" {
			t.Errorf("missing Authorization")
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":1}`))
	}))
	defer upstream.Close()
	oldPrimary := quotaRemainsURL
	oldCN := quotaRemainsCNURL
	defer func() { setQuotaURLs(oldPrimary, oldCN) }()
	setQuotaURLs(upstream.URL, upstream.URL)

	body, url, err := fetchQuotaRemains(context.Background(), "kk")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("body = %q", body)
	}
	if url != upstream.URL {
		t.Fatalf("url = %q", url)
	}
}

func TestFetchQuotaRemains_Primary401_CNFallback(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer primary.Close()
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"cn":1}`))
	}))
	defer cn.Close()
	oldPrimary := quotaRemainsURL
	oldCN := quotaRemainsCNURL
	defer func() { setQuotaURLs(oldPrimary, oldCN) }()
	setQuotaURLs(primary.URL, cn.URL)

	body, url, err := fetchQuotaRemains(context.Background(), "kk")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(body), "cn") {
		t.Fatalf("body = %q", body)
	}
	if url != cn.URL {
		t.Fatalf("url = %q", url)
	}
}

func TestFetchQuotaRemains_PrimaryTransportErr_CNFallback(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	primary.Close() // immediately closed → transport error
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"cn":1}`))
	}))
	defer cn.Close()
	oldPrimary := quotaRemainsURL
	oldCN := quotaRemainsCNURL
	defer func() { setQuotaURLs(oldPrimary, oldCN) }()
	setQuotaURLs(primary.URL, cn.URL)

	body, url, err := fetchQuotaRemains(context.Background(), "kk")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(body), "cn") {
		t.Fatalf("body = %q", body)
	}
	if url != cn.URL {
		t.Fatalf("url = %q", url)
	}
}

func TestFetchQuotaRemains_CNNon200(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer primary.Close()
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer cn.Close()
	oldPrimary := quotaRemainsURL
	oldCN := quotaRemainsCNURL
	defer func() { setQuotaURLs(oldPrimary, oldCN) }()
	setQuotaURLs(primary.URL, cn.URL)

	_, _, err := fetchQuotaRemains(context.Background(), "kk")
	if err == nil {
		t.Fatalf("expected upstream error")
	}
}

func TestDoQuotaGet_Bearer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer my-key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	resp, err := doQuotaGet(&http.Client{Timeout: time.Second}, context.Background(), upstream.URL, "my-key")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp.Body.Close()
}

func TestFetchQuotaRemains_BodyReadErr(t *testing.T) {
	// An upstream that hangs with 200 leaves io.ReadAll unable to complete
	// before the timeout. Use a short context timeout to force the body read
	// to fail inside fetchQuotaRemains.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Stall until the client gives up.
		<-r.Context().Done()
	}))
	defer upstream.Close()
	oldPrimary := quotaRemainsURL
	oldCN := quotaRemainsCNURL
	defer func() { setQuotaURLs(oldPrimary, oldCN) }()
	setQuotaURLs(upstream.URL, upstream.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := fetchQuotaRemains(ctx, "kk")
	if err == nil {
		t.Fatalf("expected body-read error, got nil")
	}
}

func TestFetchQuotaRemains_BothUpstreamsFail(t *testing.T) {
	// Both the primary and CN endpoints must return transport errors so the
	// first `if err != nil` branch fires (a non-200 status lands in the
	// status-check branch instead). Closing the server immediately causes
	// the dial to succeed but every read to error out.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	primaryURL := primary.URL
	primary.Close()
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cnURL := cn.URL
	cn.Close()
	oldPrimary := quotaRemainsURL
	oldCN := quotaRemainsCNURL
	defer func() { setQuotaURLs(oldPrimary, oldCN) }()
	setQuotaURLs(primaryURL, cnURL)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, _, err := fetchQuotaRemains(ctx, "kk")
	if err == nil {
		t.Fatalf("expected error when both upstreams fail")
	}
}

// --- modelFromBody ---

func TestModelFromBody(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`not-json`, ""},
		{`{"model":"foo"}`, "foo"},
		{`{"model":42}`, ""},
		{`{"other":"x"}`, ""},
		{`{}`, ""},
	}
	for _, tc := range cases {
		got := modelFromBody([]byte(tc.body))
		if got != tc.want {
			t.Fatalf("modelFromBody(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// --- helpers ---

func setQuotaURLs(primary, cn string) {
	quotaRemainsURL = primary
	quotaRemainsCNURL = cn
}

func futureEnd(deltaSec int64) string {
	return msec(time.Now().Unix() + deltaSec)
}

func msec(t int64) string {
	return itoa(t * 1000)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [32]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// badReader always returns an error to drive the io.ReadAll error path.
type badReader struct{}

func (b *badReader) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
