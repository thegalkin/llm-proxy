package proxy

import (
	"context"
	"fmt"
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

// Each row reddens if a plausible bug drops a reported field, the 40-message
// cap, or lets message text leak into the line.
func TestRequestShape(t *testing.T) {
	reasoning := `{"role":"assistant","content":"hello","reasoning_content":"abcd"}`
	capped := `{"messages":[` + strings.TrimSuffix(strings.Repeat(`{"role":"user","content":"x"},`, 45), ",") + `]}`
	// 44 head messages then a newest assistant turn the head cap would hide.
	cappedLast := `{"messages":[` + strings.TrimSuffix(strings.Repeat(`{"role":"user","content":"x"},`, 44), ",") +
		`,{"role":"assistant","content":"","reasoning_content":"zz"}]}`
	cases := []struct {
		name, body, exact string
		wants             []string
	}{
		{
			name: "reasoning_content_and_roles",
			body: `{"tools":[{}],"messages":[{"role":"system","content":"hi"},` + reasoning + `,{"role":"user","content":"yo"}]}`,
			wants: []string{
				"tools=1", "messages=3",
				"0:system content=string/2 rc=absent/0",
				"1:assistant content=string/5 rc=string/4",
				"2:user content=string/2 rc=absent/0",
			},
		},
		{
			name: "empty_content_flag",
			body: `{"messages":[{"role":"user","content":null},{"role":"assistant","content":""},{"role":"user","content":"x"}]}`,
			wants: []string{
				"0:user content=null/0 rc=absent/0 tool_calls=0 empty=true",
				"1:assistant content=string/0 rc=absent/0 tool_calls=0 empty=true",
				"2:user content=string/1 rc=absent/0 tool_calls=0 empty=false",
			},
		},
		{
			name: "tool_calls_count",
			body: `{"messages":[{"role":"assistant","content":"a","tool_calls":[{},{}]},{"role":"user","content":"b"}]}`,
			wants: []string{
				"0:assistant content=string/1 rc=absent/0 tool_calls=2 empty=false",
				"1:user content=string/1 rc=absent/0 tool_calls=0 empty=false",
			},
		},
		{
			name:  "top_level_counts_and_stream_false",
			body:  `{"stream":false,"tools":[{},{},{}],"messages":[{"role":"user","content":"x"}]}`,
			wants: []string{"stream=false", "tools=3", "messages=1"},
		},
		{
			name:  "stream_absent",
			body:  `{"messages":[{"role":"user","content":"x"}]}`,
			wants: []string{"stream=absent"},
		},
		{
			name:  "not_json",
			body:  "not json",
			exact: "bytes=8 not-json",
		},
		{
			name:  "cap_at_40_messages",
			body:  capped,
			wants: []string{"messages=45", "39:", " … +5 more"},
		},
		{
			name:  "cap_at_40_keeps_last",
			body:  cappedLast,
			wants: []string{"messages=45", " … +5 more", "last=44:assistant", "rc=string/2"},
		},
	}
	// Fixture content/reasoning text must never appear in any rendered line.
	leaks := []string{"hello", "abcd", "hi", "yo"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := requestShape([]byte(tc.body))
			for _, leak := range leaks {
				if strings.Contains(got, leak) {
					t.Errorf("message text %q leaked into %q", leak, got)
				}
			}
			if tc.exact != "" {
				if got != tc.exact {
					t.Fatalf("requestShape(%q) = %q, want %q", tc.body, got, tc.exact)
				}
				return
			}
			wants := append([]string{"bytes=" + fmt.Sprint(len(tc.body))}, tc.wants...)
			for _, want := range wants {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
			if strings.HasPrefix(tc.name, "cap_at_40") && strings.Contains(got, "40:") {
				t.Errorf("message 40 not truncated: %q", got)
			}
		})
	}
}
