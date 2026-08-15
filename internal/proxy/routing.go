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

	if us.URLPattern == "" || us.URLPattern == "/chat/completions" {
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
