package proxy

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// --- upstream shape ---

type Upstream struct {
	Type            string
	BaseURL         string
	Model           string
	URLPattern      string
	APIKeyEnv       string
	ReasoningEffort string
	TimeoutS        int // seconds until upstream response headers; 0 = default
}

type Rule struct {
	Priority   int
	MatchExpr  string
	Upstream   Upstream
	MatchRe    *regexp.Regexp
	CaptureSrc int
	PreserveTo string
}

type Config struct {
	ListenAddr string
	DefaultUS  Upstream
	Rules      []Rule
}

// --- default upstream (when no rule matches) ---

const (
	OpencodeGoBaseURL  = "https://opencode.ai/zen/go/v1"
	opencodeZenBaseURL = "https://opencode.ai/zen/v1"
	defaultListenAddr  = "127.0.0.1:8443"
)

// resolveConfigPath picks the routing-config path. LLM_PROXY_CONFIG
// (env var) wins, otherwise falls back to the OS user config dir.
func resolveConfigPath() string {
	if v := os.Getenv("LLM_PROXY_CONFIG"); v != "" {
		return v
	}
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "llm-proxy", "config.yaml")
}

// --- routing table loader (minimal YAML) ---

// loadRoutingConfig parses ~/.config/llm-proxy/config.yaml.
func LoadRoutingConfig() Config {
	path := resolveConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("llm-proxy: routing config %s not readable (%v); using built-in defaults",
			path, err)
		return defaultConfig()
	}
	cfg, err := LoadRoutingConfigFromString(string(data))
	if err != nil {
		log.Printf("llm-proxy: routing config parse error (%v); using built-in defaults", err)
		return defaultConfig()
	}
	return cfg
}

func LoadRoutingConfigFromString(src string) (Config, error) {
	cfg := defaultConfig()
	root, err := ParseYAML(src)
	if err != nil {
		return cfg, err
	}
	if v, ok := root["listen_addr"].(string); ok && v != "" {
		cfg.ListenAddr = v
	}
	if v, ok := root["default_to"].(string); ok && v != "" {
		if typ, mdl, base, url, eff, ok := ParseToString(v); ok {
			cfg.DefaultUS.Type = typ
			cfg.DefaultUS.Model = mdl
			if base != "" {
				cfg.DefaultUS.BaseURL = base
			}
			if url != "" {
				cfg.DefaultUS.URLPattern = url
			}
			if eff != "" {
				cfg.DefaultUS.ReasoningEffort = eff
			}
		}
	}
	if v, ok := root["default_base_url"].(string); ok && v != "" {
		cfg.DefaultUS.BaseURL = v
	}
	if v, ok := root["default_url_pattern"].(string); ok && v != "" {
		cfg.DefaultUS.URLPattern = v
	}
	if v, ok := root["default_reasoning_effort"].(string); ok && v != "" {
		cfg.DefaultUS.ReasoningEffort = v
	}

	if v, ok := root["default_timeout_s"].(int64); ok && v > 0 {
		cfg.DefaultUS.TimeoutS = int(v)
	}

	if rules, ok := root["rules"].([]any); ok {
		for _, item := range rules {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			from, _ := m["from"].(string)
			to, _ := m["to"].(string)
			if from == "" || to == "" {
				continue
			}
			priority, _ := m["priority"].(int64)
			eff, _ := m["reasoning_effort"].(string)
			base, _ := m["base_url"].(string)
			url, _ := m["url_pattern"].(string)
			us, preserveTo := buildUpstreamFromTo(to, base, url, eff, cfg.DefaultUS)
			if v, ok := m["timeout_s"].(int64); ok && v > 0 {
				us.TimeoutS = int(v)
			} else {
				us.TimeoutS = cfg.DefaultUS.TimeoutS
			}
			matchRe, captureIdx, _ := CompileFromPattern(from)
			cfg.Rules = append(cfg.Rules, Rule{
				Priority:   int(priority),
				MatchExpr:  from,
				Upstream:   us,
				MatchRe:    matchRe,
				CaptureSrc: captureIdx,
				PreserveTo: preserveTo,
			})
		}
	}
	sortRules(cfg.Rules)
	// Built-in rules inherit the default timeout unless they set their own.
	for i := range cfg.Rules {
		if cfg.Rules[i].Upstream.TimeoutS == 0 {
			cfg.Rules[i].Upstream.TimeoutS = cfg.DefaultUS.TimeoutS
		}
	}
	log.Printf("llm-proxy: loaded %d routing rules", len(cfg.Rules))
	return cfg, nil
}

// upstreamHeaderTimeout is the default time to wait for upstream response
// headers when a rule does not set timeout_s. The upstream has been observed
// to hang indefinitely on large requests; without a bound the proxy hangs
// forever and opencode retries with an ever-growing body.
const upstreamHeaderTimeout = 120 * time.Second

// defaultConfig returns the built-in routing table. It must work out of the
// box (no config file): opencode sends "GO/<model>" for the paid subscription
// and "opencode-zen/zen/<model>" for the Zen tier. Each rule rewrites the
// model field to the canonical name the upstream serves.
func defaultConfig() Config {
	cfg := Config{
		ListenAddr: defaultListenAddr,
		DefaultUS: Upstream{
			Type:       "opencode-go",
			BaseURL:    OpencodeGoBaseURL,
			URLPattern: "/chat/completions",
		},
	}
	builtin := []struct{ from, to string }{
		{"GO/*", "opencode-go/*"},
		{"opencode-zen/zen/*", "opencode-zen zen/*"},
	}
	for i, b := range builtin {
		us, preserveTo := buildUpstreamFromTo(b.to, "", "", "", cfg.DefaultUS)
		matchRe, captureIdx, _ := CompileFromPattern(b.from)
		cfg.Rules = append(cfg.Rules, Rule{
			Priority:   (i + 1) * 10,
			MatchExpr:  b.from,
			Upstream:   us,
			MatchRe:    matchRe,
			CaptureSrc: captureIdx,
			PreserveTo: preserveTo,
		})
	}
	return cfg
}

// parseToString parses a "to:" target of the form
// "<provider> <namespace>/<model>[ <eff>]" or "<provider>/<model>[ <eff>]".
func ParseToString(s string) (typ, mdl, base, url, eff string, ok bool) {
	effTok := ""
	fields := strings.Fields(s)
	if len(fields) >= 2 {
		last := strings.ToLower(fields[len(fields)-1])
		if last == "low" || last == "high" || last == "max" {
			effTok = last
			s = strings.TrimSpace(strings.TrimSuffix(s, " "+fields[len(fields)-1]))
		}
	}
	provider := ""
	for _, c := range []string{"opencode-go", "opencode-zen", "opencode"} {
		if strings.HasPrefix(s, c+" ") || strings.HasPrefix(s, c+"/") {
			provider = c
			s = strings.TrimSpace(strings.TrimPrefix(s, c))
			break
		}
	}
	if provider == "" {
		return "", "", "", "", "", false
	}
	idx := strings.Index(s, "/")
	if idx < 0 {
		return "", "", "", "", "", false
	}
	namespace := s[:idx]
	mod := s[idx+1:]
	if provider == "opencode-zen" {
		if namespace == "" {
			return "", "", "", "", "", false
		}
		// The namespace (e.g. "zen") is a routing label, not part of the
		// model name: the zen API serves bare names like "big-pickle" or
		// "deepseek-v4-flash-free" and rejects "zen/<name>".
		typ = "opencode-zen"
		return typ, mod, "", "", effTok, true
	}
	if namespace == "" {
		typ = "opencode-go"
		return typ, mod, "", "", effTok, true
	}
	mod = namespace + "/" + mod
	typ = "opencode-go"
	return typ, mod, "", "", effTok, true
}

func buildUpstreamFromTo(to, base, url, eff string, fallback Upstream) (Upstream, string) {
	typ, mdl, _, _, parsedEff, ok := ParseToString(to)
	if !ok {
		return fallback, ""
	}
	preserveTo := ""
	if strings.HasSuffix(mdl, "*") {
		preserveTo = strings.TrimSuffix(mdl, "*")
	}
	us := Upstream{Type: typ, Model: mdl}
	if typ == "opencode-zen" {
		us.BaseURL = opencodeZenBaseURL
		us.URLPattern = "/chat/completions"
	} else {
		us.BaseURL = OpencodeGoBaseURL
		us.URLPattern = "/chat/completions"
	}
	if base != "" {
		us.BaseURL = base
	}
	if url != "" {
		us.URLPattern = url
	}
	if eff != "" {
		us.ReasoningEffort = eff
	} else if parsedEff != "" {
		us.ReasoningEffort = parsedEff
	}
	return us, preserveTo
}

func CompileFromPattern(from string) (*regexp.Regexp, int, error) {
	idx := strings.Index(from, "*")
	if idx < 0 {
		re, err := regexp.Compile("(?i)^" + regexp.QuoteMeta(from) + "$")
		return re, -1, err
	}
	prefix := regexp.QuoteMeta(from[:idx])
	suffix := from[idx+1:]
	if suffix == "" {
		re, err := regexp.Compile("(?i)^" + prefix + "(.*)$")
		return re, 1, err
	}
	suffixEsc := regexp.QuoteMeta(suffix)
	re, err := regexp.Compile("(?i)^" + prefix + "(.*)" + suffixEsc + "$")
	return re, 1, err
}

func sortRules(rules []Rule) {
	for i := 1; i < len(rules); i++ {
		j := i
		for j > 0 && rules[j-1].Priority > rules[j].Priority {
			rules[j-1], rules[j] = rules[j], rules[j-1]
			j--
		}
	}
}

func (c *Config) ResolveRule(reqID string) Upstream {
	lower := strings.ToLower(reqID)
	for _, r := range c.Rules {
		if r.MatchRe == nil {
			continue
		}
		match := r.MatchRe.FindStringSubmatch(lower)
		if match == nil {
			continue
		}
		us := r.Upstream
		if r.CaptureSrc >= 0 && r.CaptureSrc < len(match) && strings.HasSuffix(us.Model, "*") {
			prefix := strings.TrimSuffix(us.Model, "*")
			// Re-match against the original key so the capture keeps the
			// request's casing; the lowercased match is only used for
			// case-insensitive routing.
			capture := match[r.CaptureSrc]
			if orig := r.MatchRe.FindStringSubmatch(reqID); orig != nil && r.CaptureSrc < len(orig) {
				capture = orig[r.CaptureSrc]
			}
			// The separator dash may live on either side of the capture
			// (e.g. "-k3" from "opencode-go/kimi-k3*" vs "k3" from
			// "GO/k3"); normalize so the composed name never gains or
			// loses it.
			capture = strings.TrimPrefix(capture, "-")
			if capture == "" {
				us.Model = strings.TrimSuffix(prefix, "-")
			} else {
				us.Model = prefix + capture
			}
		}
		return us
	}
	return c.DefaultUS
}

func ApplyReasoningEffort(body []byte, effort string) []byte {
	if effort == "" {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, ok := m["reasoning_effort"]; ok {
		return body
	}
	m["reasoning_effort"] = effort
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
