package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"llm-proxy/internal/proxy/synthetic"
)

// Exercise the attempt's stop/advance contract with a deterministic timeout;
// ForwardSynthetic itself constructs its client with a fixed 120s header timeout.
func TestSyntheticAttempt_TimeoutCommitBoundary(t *testing.T) {
	rotationState.mu.Lock()
	previous, existed := rotationState.lastGood[synthetic.FamOpenrouter]
	rotationState.mu.Unlock()
	t.Cleanup(func() {
		rotationState.mu.Lock()
		defer rotationState.mu.Unlock()
		if existed {
			rotationState.lastGood[synthetic.FamOpenrouter] = previous
		} else {
			delete(rotationState.lastGood, synthetic.FamOpenrouter)
		}
	})

	for _, committed := range []bool{false, true} {
		name := "before_headers_advances"
		if committed {
			name = "after_headers_stops"
		}
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: syntheticTimeoutTransport(func(r *http.Request) (*http.Response, error) {
				if !committed {
					return nil, context.DeadlineExceeded
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(io.MultiReader(strings.NewReader("data: partial\n\n"), iotest.ErrReader(context.DeadlineExceeded))),
				}, nil
			})}
			recorder := httptest.NewRecorder()
			recorder.Code = 0 // Distinguish no WriteHeader from an explicit 200.
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			providers := []Provider{{Name: "synthetic-timeout", Family: synthetic.FamOpenrouter, Key: "test"}}
			target := synthetic.Target{Family: synthetic.FamOpenrouter, Model: "stealth/union-alpha"}
			stop := syntheticAttempt(client, nil, recorder, request, []byte(`{}`), target, providers, "test")
			if stop != committed {
				t.Fatalf("stop = %v after committed=%v", stop, committed)
			}
			if committed {
				if recorder.Code != http.StatusOK || recorder.Body.String() != "data: partial\n\n" {
					t.Fatalf("committed response changed: status=%d body=%q", recorder.Code, recorder.Body.String())
				}
				return
			}
			if recorder.Code != 0 || len(recorder.Header()) != 0 || recorder.Body.Len() != 0 || recorder.Flushed {
				t.Fatalf("timeout committed downstream response: %+v", recorder)
			}
			client.Transport = syntheticTimeoutTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"answer":"fallback"}`))}, nil
			})
			target.Model = "openrouter/free"
			if !syntheticAttempt(client, nil, recorder, request, []byte(`{}`), target, providers, "test") {
				t.Fatal("successful next target did not stop ladder")
			}
			if recorder.Code != http.StatusOK || recorder.Body.String() != `{"answer":"fallback"}` || recorder.Header().Get("X-LLM-Proxy-Synthetic-Step") != "openrouter:openrouter/free" {
				t.Fatalf("next target response lost: status=%d body=%q headers=%v", recorder.Code, recorder.Body.String(), recorder.Header())
			}
		})
	}
}

type syntheticTimeoutTransport func(*http.Request) (*http.Response, error)

func (f syntheticTimeoutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
