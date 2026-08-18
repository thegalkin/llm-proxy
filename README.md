# llm-proxy

Anthropic/OpenAI-compatible multi-provider reverse proxy for [opencode](https://github.com/sst/opencode) with a YAML routing table. Drop-in between opencode (or any Anthropic SDK client) and the upstream API. Single static binary, zero external dependencies.

Routes requests by the body's `model` field (or request path), rewrites the model name to what the upstream actually serves, and forwards with per-provider key failover.

## Features

- **YAML routing table** at `~/.config/llm-proxy/config.yaml` — `from:` model pattern → `to:` upstream + canonical model name. Hot-reloadable via SIGHUP (not wired yet, but the loader re-runs cleanly).
- **Built-in routing** that works out of the box with no config file:
  - `GO/<model>` → opencode-go subscription (`https://opencode.ai/zen/go/v1`), model rewritten to `<name>` (e.g. `GO/deepseek-v4-flash` → `deepseek-v4-flash`).
  - `opencode-zen/zen/<model>` → opencode-zen free tier (`https://opencode.ai/zen/v1`), model rewritten to the bare name.
  - anything else → default upstream (opencode-go).
- **Multi-key failover** for opencode-go: `OPENCODE_GO_KEY_1..16` are rotated on 401/403/429/400 and on transport errors/timeouts.
- **SSE passthrough with per-event flushing** — streamed responses are forwarded event-by-event, no buffering.
- **Response-header timeout** — the upstream is known to hang on large requests; without a bound the proxy hangs forever. Timeout is per-rule (`timeout_s`) or global (`default_timeout_s`), built-in default 120 s. SSE body streaming stays unbounded.
- **Client-disconnect cancellation** — upstream requests are created with the client's context, so abandoning a request leaves no zombie upstream connection.
- **Empty-assistant sanitizer** — drops assistant messages with an empty `content` array (the opencode-go upstream rejects them with 400).
- **`/v1/models`** — model list for provider UIs.
- **In-memory per-provider stats** — request counts by status class (2xx/429/other), failover-hit counter.

## Endpoints

| Endpoint | Method | Purpose |
| --- | --- | --- |
| `/v1/messages` | POST | Anthropic-shape requests (opencode, Claude Code, etc.) |
| `/v1/chat/completions` | POST | OpenAI-shape requests |
| `/v1/models` | GET | Model list |
| `/healthz` | GET | Liveness + rules/providers count |

## Install

Requires Go 1.23+.

```sh
git clone https://github.com/thegalkin/llm-proxy.git
cd llm-proxy
go build -trimpath -ldflags="-s -w" -o llm-proxy ./cmd/llm-proxy
cp .env.example .env
$EDITOR .env                  # paste your keys (see below)
chmod 600 .env                # required — proxy refuses startup if mode is wider

# Run directly
./llm-proxy

# Or under systemd (user unit)
cp llm-proxy.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now llm-proxy
curl -s http://127.0.0.1:8443/healthz
```

`.env` keys (loaded by `run.sh` via `ENV_FILE=$DIR/.env`; `OPENCODE_GO_KEY_N` may also come from systemd `EnvironmentFile=`):

| Variable | Required | Effect |
| --- | --- | --- |
| `OPENCODE_GO_KEY_1..16` | yes (at least one) | opencode-go subscription keys, rotated on failure |
| `OPENCODE_ZEN_KEY` | no | opencode-zen free-tier key (hides the Zen family if missing) |
| `LISTEN_ADDR` | no, default `127.0.0.1:8443` | Bind address |

## Routing config

With no `~/.config/llm-proxy/config.yaml` the built-in table (see above) applies. To customize, create the file:

```yaml
listen_addr: "127.0.0.1:8443"
default_timeout_s: 120
default_to: opencode-go/deepseek-v4-flash
default_base_url: https://opencode.ai/zen/go/v1
default_url_pattern: /v1/chat/completions
default_reasoning_effort: max

rules:
  - from: "GO/*"
    to: "opencode-go/*"
    priority: 10

  - from: "opencode-zen/zen/*"
    to: "opencode-zen zen/*"
    priority: 10

  - from: "custom/*"
    to: "passthrough https://my-upstream.example.com/v1/chat/completions"
    priority: 20
```

`from:` supports a single `*` wildcard; the captured suffix is preserved in the rewritten model when `to:` ends with `*`. `to:` syntax is `<provider>/<model>` (or `<provider> <namespace>/<model>`), with optional `reasoning_effort: low|high|max`.

Provider types: `opencode-go`, `opencode-zen`, `passthrough` (direct URL, auth passthrough, no model rewrite).

## Configure opencode

Point opencode's provider at the proxy:

```jsonc
// ~/.config/opencode/opencode.json
"provider": {
  "llm-proxy": {
    "npm": "@ai-sdk/anthropic",
    "name": "llm-proxy",
    "options": { "baseURL": "http://127.0.0.1:8443" },
    "models": { /* as needed */ }
  }
}
```

Models are addressed with their routing prefixes: `GO/deepseek-v4-flash`, `opencode-zen/zen/big-pickle`, etc. The proxy rewrites them to the canonical names upstream.

## Upstream timeouts and failover

The opencode.ai upstream has been observed to hang for many minutes on large (~1 MB) requests. The proxy now:

- bounds the wait for upstream **response headers** (`timeout_s` per rule, `default_timeout_s` at the top level, built-in default 120 s). SSE body streaming stays unbounded;
- **fails over to the next key** when an attempt times out or dies on a transport error (previously only 401/403/429 triggered failover);
- returns **504** to the client when all keys time out (429 is still used when all keys answered 401/403/429);
- **cancels the upstream request when the client disconnects** — upstream requests are created with the client's context, so opencode abandoning a request no longer leaves zombie upstream connections.

## Safety

- `.env` is `chmod 600`; the proxy does not log its contents.
- By default binds to `127.0.0.1` only — the bundled systemd unit sets `LISTEN_ADDR=127.0.0.1:8443`.
- Upstream responses are passed through unchanged — status, headers, body.

## Development

Pre-commit expectations:

- `go build ./...` exits 0 with no warnings.
- `go test ./...` is green.
- `rg -n 'sk-cp-[A-Za-z0-9_-]{20,}' .env.example` returns nothing.
- `rg -n 'thegalkin|/home/thegalkin' --glob '*.go'` returns nothing.

## License

MIT.
