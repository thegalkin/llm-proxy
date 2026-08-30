package proxy

import (
	"encoding/json"
	"strings"
)

// --- request pre-processing: figure out the routing key + rewrite model ---

type RoutingDecision struct {
	Upstream      Upstream
	RewrittenBody []byte
	RuleName      string
}

const (
	HeaderRoutingTag = "X-LLM-Proxy-Routing"
)

func Decide(cfg *Config, body []byte, requestPath string, hostProvider string) RoutingDecision {
	model := ""
	mObj := map[string]any{}
	if json.Unmarshal(body, &mObj) == nil {
		if s, ok := mObj["model"].(string); ok {
			model = s
		}
	}

	var key string
	switch {
	case model != "" && strings.Contains(model, "/"):
		key = model
	case hostProvider != "" && model != "":
		key = hostProvider + "/" + model
	case hostProvider != "":
		key = hostProvider + "/"
	default:
		key = requestPath
	}

	us := cfg.ResolveRule(key)
	rd := RoutingDecision{Upstream: us, RuleName: key}

	// Only Anthropic clients (POST /v1/messages) may flip the upstream to the
	// Anthropic endpoint. OpenAI clients on /v1/chat/completions must keep the
	// OpenAI URL even when their body carries content-part arrays ({"type":"text"})
	// — those are valid in OpenAI bodies too, and flipping would make the proxy
	// return an Anthropic-shaped response that OpenAI clients cannot parse.
	if (us.URLPattern == "" || us.URLPattern == "/chat/completions") && strings.HasSuffix(requestPath, "/messages") {
		if detectAnthropicShape(body) {
			us.URLPattern = "/v1/messages"
			rd.Upstream = us
		}
	}

	if us.Model != "" && model != "" && us.Model != model {
		mObj["model"] = us.Model
		newBody, err := json.Marshal(mObj)
		if err != nil {
			rd.RewrittenBody = body
			return rd
		}
		rd.RewrittenBody = newBody
	} else if model != "" && (us.Type == "opencode-go" || us.Type == "opencode-zen" || us.Type == "openrouter") {
		// Strip a leading "<provider>/" or "<provider>/zen/" prefix from
		// the body model — these upstreams serve bare names like
		// "deepseek-v4-flash" or "big-pickle" and reject the prefixed
		// form with 401 ModelError. Anthropic keeps the bare name.
		// openrouter also receives the bare "<vendor>/<name>" id (the
		// slash inside the id is part of the name and must stay).
		prefixes := []string{us.Type + "/zen/", us.Type + "/"}
		bare := model
		for _, p := range prefixes {
			if strings.HasPrefix(bare, p) {
				bare = strings.TrimPrefix(bare, p)
				break
			}
		}
		if bare != model {
			mObj["model"] = bare
			newBody, err := json.Marshal(mObj)
			if err != nil {
				rd.RewrittenBody = body
			} else {
				rd.RewrittenBody = newBody
			}
		} else {
			rd.RewrittenBody = body
		}
	} else {
		rd.RewrittenBody = body
	}
	rd.RewrittenBody = ApplyReasoningEffort(rd.RewrittenBody, us.ReasoningEffort)
	return rd
}

func detectAnthropicShape(body []byte) bool {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	if _, ok := m["system"]; ok {
		return true
	}
	msgsAny, ok := m["messages"].([]any)
	if !ok {
		return false
	}
	for _, raw := range msgsAny {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if content, ok := msg["content"].([]any); ok {
			for _, c := range content {
				if blk, ok := c.(map[string]any); ok {
					if t, _ := blk["type"].(string); t == "text" || t == "image" || t == "tool_use" || t == "tool_result" {
						return true
					}
				}
			}
		}
	}
	return false
}
