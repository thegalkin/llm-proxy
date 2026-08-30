package proxy_test

import (
	"net/url"
	"testing"

	"llm-proxy/internal/proxy"
)

func TestLoadUpstreamProxy_ConfigOnly(t *testing.T) {
	proxy.ResetUpstreamProxyForTest()

	specs := []proxy.UpstreamProxySpec{
		{URL: "http://127.0.0.1:7897", For: []string{"openrouter"}},
	}
	if err := proxy.LoadUpstreamProxy("", specs); err != nil {
		t.Fatalf("LoadUpstreamProxy: %v", err)
	}

	got := proxy.UpstreamProxyURLForFamily("openrouter")
	if got == nil {
		t.Fatal("openrouter family got nil proxy, want http://127.0.0.1:7897")
	}
	if got.Host != "127.0.0.1:7897" || got.Scheme != "http" {
		t.Fatalf("openrouter proxy = %s://%s, want http://127.0.0.1:7897", got.Scheme, got.Host)
	}

	for _, family := range []string{"minimax", "opencode-go", "ollama", "passthrough"} {
		if u := proxy.UpstreamProxyURLForFamily(family); u != nil {
			t.Errorf("family %s should be nil, got %s://%s", family, u.Scheme, u.Host)
		}
	}
}

func TestLoadUpstreamProxy_EnvDefaultAppliesToAll(t *testing.T) {
	proxy.ResetUpstreamProxyForTest()
	t.Setenv("LLM_PROXY_HTTP_PROXY", "http://127.0.0.1:7897")
	t.Setenv("LLM_PROXY_HTTP_PROXY_FAMILY_OPENROUTER", "")
	if err := proxy.LoadUpstreamProxy("http://127.0.0.1:7897", nil); err != nil {
		t.Fatalf("LoadUpstreamProxy: %v", err)
	}
	for _, family := range []string{"minimax", "opencode-go", "openrouter", "ollama"} {
		u := proxy.UpstreamProxyURLForFamily(family)
		if u == nil {
			t.Errorf("family %s expected to inherit default proxy", family)
		}
	}
}

func TestLoadUpstreamProxy_PerFamilyEnvWins(t *testing.T) {
	proxy.ResetUpstreamProxyForTest()
	t.Setenv("LLM_PROXY_HTTP_PROXY", "http://127.0.0.1:7897")
	t.Setenv("LLM_PROXY_HTTP_PROXY_FAMILY_OPENROUTER", "http://10.0.0.5:1080")
	if err := proxy.LoadUpstreamProxy("http://127.0.0.1:7897", nil); err != nil {
		t.Fatalf("LoadUpstreamProxy: %v", err)
	}

	if u := proxy.UpstreamProxyURLForFamily("openrouter"); u == nil || u.Host != "10.0.0.5:1080" {
		t.Errorf("openrouter should be overridden to 10.0.0.5:1080, got %v", u)
	}
	if u := proxy.UpstreamProxyURLForFamily("minimax"); u == nil || u.Host != "127.0.0.1:7897" {
		t.Errorf("minimax should still be default, got %v", u)
	}
}

func TestLoadUpstreamProxy_RejectsBadScheme(t *testing.T) {
	proxy.ResetUpstreamProxyForTest()
	err := proxy.LoadUpstreamProxy("ftp://nope", nil)
	if err == nil {
		t.Fatal("expected error for ftp:// scheme, got nil")
	}
}

func TestLoadUpstreamProxy_ConfigParsedURL(t *testing.T) {
	proxy.ResetUpstreamProxyForTest()
	// spec.URL is the raw config string; LoadUpstreamProxy should parse it
	// (no need to pre-fill .parsed field).
	specs := []proxy.UpstreamProxySpec{
		{URL: "socks5://127.0.0.1:1080", For: []string{"openrouter"}},
	}
	if err := proxy.LoadUpstreamProxy("", specs); err != nil {
		t.Fatalf("LoadUpstreamProxy: %v", err)
	}
	u := proxy.UpstreamProxyURLForFamily("openrouter")
	if u == nil {
		t.Fatal("nil proxy for openrouter")
	}
	if u.Scheme != "socks5" {
		t.Errorf("scheme = %s, want socks5", u.Scheme)
	}
}

func TestUpstreamClientHonoursProxy(t *testing.T) {
	proxy.ResetUpstreamProxyForTest()
	if err := proxy.LoadUpstreamProxy("", []proxy.UpstreamProxySpec{
		{URL: "http://127.0.0.1:7897", For: []string{"openrouter"}},
	}); err != nil {
		t.Fatalf("LoadUpstreamProxy: %v", err)
	}
	_, tr := proxy.UpstreamClientForTest(60, "openrouter")
	if tr.Proxy == nil {
		t.Fatal("openrouter transport has nil Proxy; expected non-nil http.ProxyURL")
	}
	_, tr = proxy.UpstreamClientForTest(60, "minimax")
	if tr.Proxy != nil {
		t.Fatalf("minimax transport has non-nil Proxy func; expected nil (no proxy for that family)")
	}
}

// --- ensure URL import is referenced even if other tests trimmed ---
var _ = url.Parse