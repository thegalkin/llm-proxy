package proxy_test

import (
	"testing"

	"llm-proxy/internal/proxy"
)

func TestParseToString_ParseOnly(t *testing.T) {
	cases := []struct {
		in      string
		wantT   string
		wantM   string
		wantEff string
		wantOk  bool
	}{
		{"minimax sub/MiniMax-M3", "minimax", "MiniMax-M3", "", true},
		// The star must keep the canonical "MiniMax-" prefix so a captured
		// "m3" becomes "MiniMax-m3" (not "minimaxm3"). ResolveRule trims
		// a leading dash from the capture either way.
		{"minimax sub/minimax*", "minimax", "MiniMax-*", "", true},
		{"minimax sub/minimax-m3", "minimax", "MiniMax-m3", "", true},
		{"opencode-go/deepseek-v4-flash", "opencode-go", "deepseek-v4-flash", "", true},
		{"opencode-go/deepseek max", "opencode-go", "deepseek", "max", true},
		{"opencode-go/deepseek low", "opencode-go", "deepseek", "low", true},
		{"unknown", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			typ, mdl, _, _, eff, ok := proxy.ParseToString(tc.in)
			if ok != tc.wantOk {
				t.Errorf("ok=%v, want %v (typ=%q mdl=%q eff=%q)", ok, tc.wantOk, typ, mdl, eff)
			}
			if typ != tc.wantT {
				t.Errorf("typ=%q, want %q", typ, tc.wantT)
			}
			if mdl != tc.wantM {
				t.Errorf("mdl=%q, want %q", mdl, tc.wantM)
			}
			if eff != tc.wantEff {
				t.Errorf("eff=%q, want %q", eff, tc.wantEff)
			}
		})
	}
}

func TestCompileFromPattern(t *testing.T) {
	cases := []struct {
		from    string
		key     string
		wantOk  bool
		wantCap string
	}{
		{"opencode-go/minimax*", "opencode-go/minimax-m3", true, "-m3"},
		{"opencode-go/minimax*", "opencode-go/minimax-m2.7", true, "-m2.7"},
		{"opencode-go/minimax*", "opencode-go/kimi-k3", false, ""},
		{"opencode-go/*", "opencode-go/kimi-k3", true, "kimi-k3"},
		{"*", "anything", true, "anything"},
		{"opencode-go/kimi-k3", "opencode-go/kimi-k3", true, ""},
		{"opencode-go/kimi-k3", "opencode-go/glm-5.2", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.from+" vs "+tc.key, func(t *testing.T) {
			re, cap, err := proxy.CompileFromPattern(tc.from)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			m := re.FindStringSubmatch(tc.key)
			gotOk := m != nil
			if gotOk != tc.wantOk {
				t.Errorf("matched=%v, want %v", gotOk, tc.wantOk)
			}
			if tc.wantCap != "" && cap < len(m) {
				if m[cap] != tc.wantCap {
					t.Errorf("cap[%d]=%q, want %q", cap, m[cap], tc.wantCap)
				}
			}
		})
	}
}
