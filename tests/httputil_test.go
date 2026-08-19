package proxy_test

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"
	"testing"

	"llm-proxy/internal/proxy"
)

func TestIsRateLimitError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"429 rate_limit_error", 429, `{"error":{"type":"rate_limit_error"}}`, true},
		{"429 token plan usage limit", 429, `token plan usage limit reached`, true},
		{"429 usage limit reached", 429, `usage limit reached`, true},
		{"429 other error", 429, `{"error":{"type":"invalid_request_error"}}`, false},
		{"200 rate_limit_error", 200, `{"error":{"type":"rate_limit_error"}}`, false},
		{"429 empty body", 429, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxy.IsRateLimitError(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("isRateLimitError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestCopyHeadersSkipsHopByHop(t *testing.T) {
	src := http.Header{
		"Connection":        {"close"},
		"Keep-Alive":        {"timeout=5"},
		"Transfer-Encoding": {"chunked"},
		"X-Custom":          {"a", "b"},
		"Content-Type":      {"application/json"},
	}
	dst := http.Header{}
	proxy.CopyHeaders(dst, src)
	for _, hop := range []string{"Connection", "Keep-Alive", "Transfer-Encoding"} {
		if _, ok := dst[http.CanonicalHeaderKey(hop)]; ok {
			t.Errorf("hop-by-hop header %s must not be copied", hop)
		}
	}
	if got := dst.Values("X-Custom"); len(got) != 2 {
		t.Errorf("X-Custom = %v, want [a b]", got)
	}
	if got := dst.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestContainsHeader(t *testing.T) {
	h := http.Header{"X-Accel-Buffering": {"no"}}
	if !proxy.ContainsHeader(h, "x-accel-buffering") {
		t.Error("containsHeader should be case-insensitive")
	}
	if proxy.ContainsHeader(h, "X-Missing") {
		t.Error("containsHeader matched a missing header")
	}
}

func TestPeekBufCaps(t *testing.T) {
	peek := make([]byte, 0, 4)
	p := &proxy.PeekBuf{Peek: &peek, Cap: 4}
	if n, _ := p.Write([]byte("hello world")); n != 11 {
		t.Errorf("Write returned %d, want 11", n)
	}
	if string(peek) != "hell" {
		t.Errorf("peek = %q, want %q", peek, "hell")
	}
	if len(peek) != 4 {
		t.Errorf("len(peek) = %d, want 4", len(peek))
	}
	// further writes must not grow past cap
	p.Write([]byte("x"))
	if len(peek) != 4 {
		t.Errorf("peek grew past cap: %d", len(peek))
	}
}

func TestStreamSSEDropsDone(t *testing.T) {
	src := strings.NewReader("data: hello\n\n" + "data: [DONE]\n\n" + "data: world\n\n")
	var out bytes.Buffer
	fw := &proxy.FlushWriter{WW: bufio.NewWriter(&out)}
	if err := proxy.StreamSSE(fw, src); err != nil {
		t.Fatalf("streamSSE: %v", err)
	}
	want := "data: hello\n\ndata: world\n\n"
	if out.String() != want {
		t.Errorf("streamSSE output = %q, want %q", out.String(), want)
	}
}

func TestStreamSSETrailingPartialEvent(t *testing.T) {
	src := strings.NewReader("data: hello\n\n" + "data: bye\n")
	var out bytes.Buffer
	fw := &proxy.FlushWriter{WW: bufio.NewWriter(&out)}
	if err := proxy.StreamSSE(fw, src); err != nil {
		t.Fatalf("streamSSE: %v", err)
	}
	want := "data: hello\n\ndata: bye\n"
	if out.String() != want {
		t.Errorf("streamSSE output = %q, want %q", out.String(), want)
	}
}
