package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// IsRateLimitError only fires on a real 429 AND a body that contains
// one of the well-known rate-limit markers. A bare 429 with no body or
// a generic body must NOT trip it (the proxy would otherwise rotate to
// the next key for a transient 429).
func TestIsRateLimitError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"non-429 false", 500, `{"error":"server"}`, false},
		{"non-429 200 false", 200, `rate_limit_error`, false},
		{"429 + rate_limit_error", 429, `{"type":"error","error":{"type":"rate_limit_error"}}`, true},
		{"429 + token plan usage limit", 429, `token plan usage limit`, true},
		{"429 + usage limit reached", 429, `usage limit reached`, true},
		{"429 + GoUsageLimitError", 429, `GoUsageLimitError`, true},
		{"429 empty body false", 429, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRateLimitError(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("IsRateLimitError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// IsModelShapeError returns true when a 4xx body means "the upstream
// does not serve this model" — distinct from quota/auth errors which
// would let the proxy try the next key in the family.
func TestIsModelShapeError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		// Status outside the 4xx band → false.
		{"500 not 4xx", 500, `ModelError`, false},
		// Quota/auth signals → false (keep rotating).
		{"GoUsageLimitError", 401, `GoUsageLimitError`, false},
		{"usage limit", 400, `usage limit`, false},
		{"rate_limit_error", 429, `rate_limit_error`, false}, // 429 not in scope
		{"creditsError", 400, `creditsError`, false},
		{"authError", 403, `authError`, false},
		// "Model  is not supported" with the empty-model anomaly → false.
		{"model empty name", 401, `Model  is not supported`, false},
		{"model triple-space name", 401, `Model   is not supported`, false},
		// Real model-shape errors → true.
		{"ModelError", 401, `ModelError`, true},
		{"is not supported", 400, `this model is not supported`, true},
		{"model_not_found", 400, `model_not_found`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsModelShapeError(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("IsModelShapeError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// CopyHeaders must copy every header EXCEPT the hop-by-hop headers
// (Connection, Keep-Alive, etc.) which are the connection's concern,
// not the upstream's.
func TestCopyHeaders(t *testing.T) {
	t.Run("canonical headers copied", func(t *testing.T) {
		src := http.Header{
			"X-Custom":     {"a", "b"},
			"Content-Type": {"application/json"},
		}
		dst := http.Header{}
		CopyHeaders(dst, src)
		if got := dst.Values("X-Custom"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("X-Custom = %v, want [a b]", got)
		}
		if got := dst.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
	})
	t.Run("hop-by-hop headers skipped", func(t *testing.T) {
		src := http.Header{
			"Connection":        {"close"},
			"Keep-Alive":        {"timeout=5"},
			"Transfer-Encoding": {"chunked"},
			"Upgrade":           {"websocket"},
			"X-Keep":            {"yes"},
		}
		dst := http.Header{}
		CopyHeaders(dst, src)
		for _, hop := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade"} {
			if got := dst.Get(hop); got != "" {
				t.Errorf("%s = %q, want empty (hop-by-hop)", hop, got)
			}
		}
		if got := dst.Get("X-Keep"); got != "yes" {
			t.Errorf("X-Keep = %q, want yes", got)
		}
	})
}

// ContainsHeader is a case-insensitive probe over http.Header keys.
// Empty header map, canonical name, and a non-canonical lookup must
// all behave correctly.
func TestContainsHeader(t *testing.T) {
	t.Run("absent header", func(t *testing.T) {
		if ContainsHeader(http.Header{}, "X-Anything") {
			t.Error("empty header should not match")
		}
	})
	t.Run("case-insensitive match", func(t *testing.T) {
		h := http.Header{"X-Accel-Buffering": {"no"}}
		if !ContainsHeader(h, "x-accel-buffering") {
			t.Error("case-insensitive match failed")
		}
		if !ContainsHeader(h, "X-Accel-Buffering") {
			t.Error("exact case match failed")
		}
	})
	t.Run("multiple values still counts as present", func(t *testing.T) {
		h := http.Header{"X-Multi": {"a", "b", "c"}}
		if !ContainsHeader(h, "x-multi") {
			t.Error("multi-valued header should be detected")
		}
	})
}

// newFlushWriter attaches an http.Flusher to the writer if the
// underlying ResponseWriter supports it. We exercise both branches:
// a ResponseWriter that implements Flusher, and one that doesn't.
func TestNewFlushWriter(t *testing.T) {
	t.Run("responsewriter with flusher", func(t *testing.T) {
		fw := newFlushWriter(&flushingRW{})
		if fw.f == nil {
			t.Error("fw.f should be set when ResponseWriter implements http.Flusher")
		}
	})
	t.Run("plain responsewriter", func(t *testing.T) {
		fw := newFlushWriter(&plainRW{})
		if fw.f != nil {
			t.Error("fw.f must be nil when ResponseWriter does not implement http.Flusher")
		}
	})
}

// FlushWriter.Write pipes through the underlying bufio.Writer and
// flushes both the writer and the http.Flusher.
func TestFlushWriterWrite(t *testing.T) {
	t.Run("write below peek cap", func(t *testing.T) {
		fr := &flushingRW{}
		fw := newFlushWriter(fr)
		n, err := fw.Write([]byte("hello"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != 5 {
			t.Errorf("n = %d, want 5", n)
		}
		if !fr.flushed {
			t.Error("underlying ResponseWriter.Flush was not called")
		}
	})
	t.Run("write failure surfaces", func(t *testing.T) {
		fw := &FlushWriter{
			w:  &flushingRW{},
			WW: bufio.NewWriter(&errWriter{}),
		}
		_, err := fw.Write([]byte("x"))
		if err == nil {
			t.Error("Write must surface writer errors")
		}
	})
}

// PeekBuf caps the bytes it retains; the Write return value is the
// full input length regardless of how much got peeked.
func TestPeekBufWriteLarge(t *testing.T) {
	peek := make([]byte, 0, 4)
	p := &PeekBuf{Peek: &peek, Cap: 4}
	n, err := p.Write([]byte("hello world"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 11 {
		t.Errorf("Write returned %d, want 11 (return value is the input length, not the peek amount)", n)
	}
	if string(peek) != "hell" {
		t.Errorf("peek = %q, want %q", peek, "hell")
	}
}

// StreamSSE walks a byte stream and forwards "data: ..." events to the
// FlushWriter, dropping "[DONE]" terminators and preserving partial
// trailing events.
func TestStreamSSE(t *testing.T) {
	t.Run("basic stream", func(t *testing.T) {
		src := strings.NewReader("data: hello\n\ndata: world\n\n")
		var out bytes.Buffer
		fw := &FlushWriter{WW: bufio.NewWriter(&out)}
		if err := StreamSSE(fw, src); err != nil {
			t.Fatalf("StreamSSE: %v", err)
		}
		want := "data: hello\n\ndata: world\n\n"
		if out.String() != want {
			t.Errorf("output = %q, want %q", out.String(), want)
		}
	})
	t.Run("data prefix only", func(t *testing.T) {
		// SSE comments / non-data fields are forwarded verbatim.
		src := strings.NewReader(":heartbeat\n\ndata: actual\n\n")
		var out bytes.Buffer
		fw := &FlushWriter{WW: bufio.NewWriter(&out)}
		if err := StreamSSE(fw, src); err != nil {
			t.Fatalf("StreamSSE: %v", err)
		}
		if !strings.Contains(out.String(), "actual") {
			t.Errorf("data line not forwarded: %q", out.String())
		}
	})
	t.Run("drops [DONE]", func(t *testing.T) {
		src := strings.NewReader("data: hello\n\ndata: [DONE]\n\ndata: world\n\n")
		var out bytes.Buffer
		fw := &FlushWriter{WW: bufio.NewWriter(&out)}
		if err := StreamSSE(fw, src); err != nil {
			t.Fatalf("StreamSSE: %v", err)
		}
		if strings.Contains(out.String(), "[DONE]") {
			t.Errorf("[DONE] was not dropped: %q", out.String())
		}
	})
	t.Run("malformed event forwarded as-is", func(t *testing.T) {
		// No "data:" prefix → the event is forwarded verbatim.
		src := strings.NewReader("event: ping\n\n")
		var out bytes.Buffer
		fw := &FlushWriter{WW: bufio.NewWriter(&out)}
		if err := StreamSSE(fw, src); err != nil {
			t.Fatalf("StreamSSE: %v", err)
		}
		if !strings.Contains(out.String(), "event: ping") {
			t.Errorf("malformed event lost: %q", out.String())
		}
	})
	t.Run("trailing partial event", func(t *testing.T) {
		// The loop breaks on io.EOF before a "\n\n" terminator; the
		// trailing partial event must still be flushed.
		src := strings.NewReader("data: hello\n\ndata: bye\n")
		var out bytes.Buffer
		fw := &FlushWriter{WW: bufio.NewWriter(&out)}
		if err := StreamSSE(fw, src); err != nil {
			t.Fatalf("StreamSSE: %v", err)
		}
		want := "data: hello\n\ndata: bye\n"
		if out.String() != want {
			t.Errorf("trailing partial event lost: got %q, want %q", out.String(), want)
		}
	})
	t.Run("non-EOF read error returned", func(t *testing.T) {
		fw := &FlushWriter{WW: bufio.NewWriter(io.Discard)}
		err := StreamSSE(fw, &failingReader{})
		if err == nil {
			t.Error("StreamSSE must surface read errors")
		}
	})
}

// StreamSSE surfaces a write error from the destination FlushWriter.
func TestStreamSSEWriteError(t *testing.T) {
	src := strings.NewReader("data: hello\n\n")
	fw := &FlushWriter{WW: bufio.NewWriter(&errWriter{})}
	if err := StreamSSE(fw, src); err == nil {
		t.Error("StreamSSE must surface destination Write errors")
	}
}

// FlushWriter.Write surfaces the bufio.Writer's Write error path. With a
// 1-byte buffer, two-byte input overflows and the implicit flush
// returns the underlying error from bufio.Write itself (before the
// explicit Flush call).
func TestFlushWriterWriteBufioError(t *testing.T) {
	fw := &FlushWriter{
		w:  &flushingRW{},
		WW: bufio.NewWriterSize(&errWriter{}, 1),
	}
	_, err := fw.Write([]byte("xy"))
	if err == nil {
		t.Error("Write must surface underlying writer errors")
	}
}

// flushingRW implements http.ResponseWriter + http.Flusher for the
// newFlushWriter happy path.
type flushingRW struct {
	flushed bool
	hdr     http.Header
}

func (f *flushingRW) Header() http.Header         { return f.hdr }
func (f *flushingRW) Write(p []byte) (int, error) { return len(p), nil }
func (f *flushingRW) WriteHeader(int)             {}
func (f *flushingRW) Flush()                      { f.flushed = true }

// plainRW implements http.ResponseWriter only — no http.Flusher.
type plainRW struct{ hdr http.Header }

func (p *plainRW) Header() http.Header       { return p.hdr }
func (p *plainRW) Write([]byte) (int, error) { return 0, nil }
func (p *plainRW) WriteHeader(int)           {}

// errWriter always fails, used to exercise FlushWriter's error path.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("nope") }

// failingReader always returns a non-EOF error to drive StreamSSE's
// error path.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
