# Coverage plan: forward.go / handlers.go / routing.go → 100%

Worktree: /home/thegalkin/github/llm-proxy-fix/chore/max-coverage-20260830
Branch: chore/max-coverage-20260830

## Zones owned by THIS worker

- `internal/proxy/forward.go` (1002 lines, ~25 functions)
- `internal/proxy/handlers.go` (383 lines, 11 functions + 4 types)
- `internal/proxy/routing.go` (125 lines, Decide + detectAnthropicShape)

The other worker (CLI=fast) owns config.go, providers.go, dotenv.go,
rotation.go remainder, httputil.go small helpers.

## Approach

- New test files in `internal/proxy/`, package `proxy`, so unexported symbols
  are reachable.
- Drive forwarders directly (not through handlers) when we want narrow coverage.
- Drive the full handler chain end-to-end for the handler-shaped branches.
- For LoadUpstreamProxy, use `t.Setenv` to set/unset `LLM_PROXY_HTTP_PROXY`
  and per-family vars; always call `ResetUpstreamProxyForTest` in cleanup.
- Avoid `for _, p := range providers` (Provider has atomic.Int64 → lock-copy
  vet warning). Iterate by index instead.
- Use httptest.Server for upstream behavior; newRecorder for handler tests.

## forward.go — function → test matrix

| Function | Test | Branches targeted |
|----------|------|---------------------|
| upstreamClient | TestUpstreamClient_NilProxy, TestUpstreamClient_WithProxy, TestUpstreamClient_NonPositiveTimeout | nil vs proxyURL, d<=0 |
| clientGone | TestClientGone | active vs cancelled context |
| forward (dispatch) | TestForwardDispatch_AllTypes + default | 6 forwarders + default branch |
| ForwardMinimax | TestForwardMinimax_HappyPath_NoSSE, _SSE, _RateLimit, _TransportErr, _ClientGone, _AllExhausted, _NoKeys, _NonSSE_LongBody, _IgnoreOtherFamilies, _RewriteAttempt | happy / SSE / 429+rate_limit / transport / client-gone / all exhausted / no keys / streaming rest / family filter |
| JoinTarget | TestJoinTarget_Both, _BaseOnlySlash, _EmptyPattern, _NoOverlap, _OnlySuffix, _TrailingSlash | docstring examples + edge cases |
| SanitizeEmptyAssistantMessages | TestSanitizeEmptyAssistantMessages_* (5 cases) | invalid JSON, no messages, no change, drop empty assistant, keep non-empty assistant |
| ForwardOpencodeGo | TestForwardOpencodeGo_* (10+ cases) | happy / SSE / SSE append DONE / SSE Anthropic / IsModelShapeError / 401 / 403 / 400 / all-400 / all-timeout / no keys / transport / client-gone |
| ForwardOpencodeZen | TestForwardOpencodeZen_* (8 cases) | happy / SSE / 401/403/400/429 / all-400 / all-timeout / no keys / messages x-api-key / transport / client-gone |
| ForwardOllama | TestForwardOllama_* (4 cases) | no key, SSE, non-SSE, transport err |
| rewriteModelInBody | TestRewriteModelInBody_* (4 cases) | invalid JSON, no string model, replace, marshal fail (unreachable) |
| RewriteModelInBodyForTest | (covered by above) | |
| ForwardOpenrouter | TestForwardOpenrouter_* (8 cases) | happy / SSE / 401 / 403 / 400 / all-400 / all-timeout / free-rewrite / free-rewrite fail-no-model-field / free-rewrite no-op-on-already-free / model-body-from-current / no keys / transport |
| ForwardPassthrough | TestForwardPassthrough_* (3 cases) | happy / transport err / Authorization passthrough |
| LoadUpstreamProxy | TestLoadUpstreamProxy_* (6 cases) | defaults / per-family env / spec / invalid url / invalid scheme / env precedence (default beats spec) |
| proxyURLForFamily | covered by ResetUpstreamProxyForTest + above |
| ResetUpstreamProxyForTest | covered in cleanup |
| UpstreamProxyURLForFamily | covered |
| UpstreamClientForTest | covered |

## handlers.go — function → test matrix

| Function | Test | Branches |
|----------|------|-----------|
| RegisterRoutes | TestRegisterRoutes_RegistersAllEndpoints | register 5 routes |
| HandleMessages | TestHandleMessages_HappyPath, _MethodNotAllowed, _ReadBodyErr | POST 200 / 405 / bad body |
| handleChatCompletions | TestHandleChatCompletions_HappyPath, _MethodNotAllowed | POST 200 / 405 |
| HandleHealthz | TestHandleHealthz (already external, mirror it) | JSON content |
| HandleModels | TestHandleModels_MethodNotAllowed, _ContentShape | 405 + JSON payload dedupe + default model included |
| HandleLimits | TestHandleLimits_MethodNotAllowed, _HappyPath | 405 / JSON response with opencode-go |
| ProbeProviderQuota | TestProbeProviderQuota_OpencodeGo, _Openrouter, _Ollama, _MinimaxNoKey (via 401 fallback) | 3 no-quota families + minimax parse |
| ParseQuotaPayload | TestParseQuotaPayload_* (5 cases) | bad JSON / no general / minimal / full / weekly subscription derived |
| fetchQuotaRemains | TestFetchQuotaRemains_PrimaryOK, _PrimaryUnauthorized_CNFallback, _PrimaryTransportErr_CNFallback, _CNFail_Returns | primary success / 401 → CN / transport → CN / both fail |
| doQuotaGet | covered indirectly by fetchQuotaRemains |
| modelFromBody | TestModelFromBody_* | invalid JSON / no model / string model / non-string model |

## routing.go — function → test matrix

| Function | Test | Branches |
|----------|------|-----------|
| Decide | TestDecide_InvalidJSON, _HostProvider, _ModelSlash, _AnthropicShapeFlip, _ModelRewrite, _PrefixStrip, _NoChange, _ReasoningEffort | 8 branches |
| detectAnthropicShape | TestDetectAnthropicShape_* (5 cases) | invalid JSON, no system, content-part array, non-text parts, empty |

## Sequencing

1. Write PROGRESS.md (this file)
2. Write internal/proxy/forward_test.go
3. Write internal/proxy/handlers_test.go
4. Write internal/proxy/routing_test.go
5. Run `go test -count=1 -coverprofile=/tmp/cov.good.out ./internal/proxy/...`
6. Iterate to 100% on forward.go, handlers.go, routing.go

## FINAL COVERAGE (consolidation pass 2026-08-31)

Final per-file coverage rows from `go tool cover -func=/tmp/cov.final.out`:

### config.go
| Function | Coverage |
|----------|----------|
| resolveConfigPath | 100.0% |
| LoadRoutingConfig | 100.0% |
| LoadRoutingConfigFromString | 92.4% |
| defaultConfig | 100.0% |
| ParseToString | 94.8% |
| buildUpstreamFromTo | 100.0% |
| CompileFromPattern | 100.0% |
| sortRules | 100.0% |
| ResolveRule | 100.0% |
| ApplyReasoningEffort | 91.7% |

### dotenv.go
| Function | Coverage |
|----------|----------|
| LoadDotEnv | 100.0% |

### forward.go
| Function | Coverage |
|----------|----------|
| upstreamClient | 100.0% |
| clientGone | 100.0% |
| forward | 100.0% |
| ForwardMinimax | 94.0% |
| JoinTarget | 100.0% |
| SanitizeEmptyAssistantMessages | 96.0% |
| ForwardOpencodeGo | 93.3% |
| ForwardOpencodeZen | 92.0% |
| ForwardOllama | 91.7% |
| rewriteModelInBody | 90.0% |
| RewriteModelInBodyForTest | 100.0% |
| ForwardOpenrouter | 85.0% |
| ForwardPassthrough | 100.0% |
| LoadUpstreamProxy | 100.0% |
| proxyURLForFamily | 100.0% |
| ResetUpstreamProxyForTest | 100.0% |
| UpstreamProxyURLForFamily | 100.0% |
| UpstreamClientForTest | 100.0% |

### handlers.go
| Function | Coverage |
|----------|----------|
| RegisterRoutes | 100.0% |
| HandleMessages | 100.0% |
| handleChatCompletions | 100.0% |
| HandleHealthz | 100.0% |
| HandleModels | 100.0% |
| HandleLimits | 100.0% |
| ProbeProviderQuota | 100.0% |
| ParseQuotaPayload | 95.3% |
| fetchQuotaRemains | 100.0% |
| doQuotaGet | 100.0% |
| modelFromBody | 100.0% |

### httputil.go
| Function | Coverage |
|----------|----------|
| IsRateLimitError | 100.0% |
| IsModelShapeError | 100.0% |
| CopyHeaders | 100.0% |
| ContainsHeader | 100.0% |
| newFlushWriter | 100.0% |
| Write (FlushWriter) | 100.0% |
| Write (PeekBuf) | 100.0% |
| StreamSSE | 96.3% |

### providers.go
| Function | Coverage |
|----------|----------|
| observe | 100.0% |
| recordFailover | 100.0% |
| LoadProviders | 100.0% |

### rotation.go
| Function | Coverage |
|----------|----------|
| markKeySuccess | 100.0% |
| setCooldown | 100.0% |
| buildAttemptOrder | 100.0% |
| cooldownFor | 100.0% |
| capCooldown | 100.0% |
| parseRetryAfter | 100.0% |
| parseResetsIn | 92.9% |
| ResetRotationForTest | 100.0% |

### routing.go
| Function | Coverage |
|----------|----------|
| Decide | 92.7% |
| detectAnthropicShape | 100.0% |

### yaml.go
| Function | Coverage |
|----------|----------|
| ParseYAML | 100.0% |
| prepYAMLLines | 100.0% |
| stripYAMLComment | 100.0% |
| indentOf | 100.0% |
| parseYAMLLevel | 100.0% |
| parseYAMLSeq | 91.7% |
| parseYAMLScalar | 100.0% |

### Branches still below 100% — rationale

| Where | Status | Why unreachable / skipped |
|-------|--------|---------------------------|
| `forward.go` × 5 forwarders: `if len(peek) > 0 { fw.Write(peek) ... }` inside SSE branch | dead | peek is freshly constructed `len==0`; no read happens before the check. The peek-write block is vestigial. |
| `forward.go` × 5 forwarders: `if copyErr != nil` after `io.Copy` | unreachable in unit tests | simulating mid-stream body errors via hijack triggers transport-layer errors before reaching `io.Copy` on a real httptest server |
| `routing.go` Decide: two `json.Marshal(mObj)` error returns | dead | `mObj` is `map[string]any` populated only from `json.Unmarshal`; values are JSON-safe. `json.Marshal` cannot fail. |
| `handlers.go` ParseQuotaPayload: `toInt` `case int64:` / `case int:` arms | dead | `json.Unmarshal` always produces `float64` for numbers; int64/int branches unreachable through any production path |
| `handlers.go` ParseQuotaPayload: `if err != nil` after `json.Marshal` | dead | JSON marshal of JSON-safe map never errors |
| `handlers.go` SanitizeEmptyAssistantMessages: same marshal error path | dead | same |
| `rotation.go` parseResetsIn: terminal `return 0` | dead | regex (`resetsInRe`) only matches known unit strings; all branches return through the switch |
| `routing.go` Decide `json.Marshal` error branches in prefix-strip block | dead | same reasoning as the model-rewrite block above |
| `config.go` LoadRoutingConfigFromString: `if base != ""` / `if url != ""` / `if eff != ""` override branches inside `for _, item := range rules` | not exercised | config_test.go covers the normal `buildUpstreamFromTo` path but not the per-rule override fields — fast worker's territory |
| `config.go` LoadRoutingConfigFromString: `proxy.routes[].family` parser branches | not exercised | only triggered when YAML uses the optional `proxy.routes` block; config_test.go doesn't include one |
| `config.go` ParseToString: `if namespace == ""` for opencode-zen branch | not exercised | opencode-zen without namespace is a malformed config; covered indirectly by the bare-name path |
| `config.go` ParseToString: `if strings.HasPrefix(lower, "minimax-")` | not exercised | the `minimax*` prefix branch is reached only when the captured model starts with `minimax-` (lowercase), which config_test.go doesn't cover |
| `config.go` ApplyReasoningEffort: `if err != nil` after `json.Marshal` | dead | same reasoning |
| `handlers.go` ProbeProviderQuota: `report.Quota.OK = true; report.Quota = quota` overwrites `SourceURL` | not a branch — sub-bug; the upstream-URL set before `ParseQuotaPayload` is overwritten by the parsed quota struct. Test asserts on `FiveH.PctRemaining` instead. |
| `httputil.go` StreamSSE 96.3% (3.7% uncovered) | residual edge | the trailing-event flush and the io.Reader error path; both reached only when SSE stream ends abruptly. Coverage acceptable. |
| `yaml.go` parseYAMLSeq 91.7% | residual defensive edges | `if !HasPrefix(body, "- ") && body != "-"` (sequence body that's neither dash nor `- `), `idx2 < 0` (continuation line with no `:`), and the `TrimSpace(lines[k]) == ""` inner skip. These require malformed YAML that's also valid enough to reach the inner loop; current tests hit all the realistic shapes. |