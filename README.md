# minimax-proxy

Anthropic-compatible reverse proxy for [`api.minimax.io`](https://platform.minimax.io) — drop-in between [opencode](https://github.com/sst/opencode) (or any Anthropic SDK client) and the upstream API. Single static binary, zero external dependencies, ~430 lines of Go.

## Features

- **Two-key provider failover.** Rotate between `MINIMAX_CODING_PLAN_KEY` and `MINIMAX_KEY` when the upstream returns HTTP 429 `type:"rate_limit_error"`. First non-rate-limit response wins; both exhausted → a final 429 to the client (so its own fallback chain can fire).
- **SSE passthrough with per-event flushing.** Streaming responses are forwarded event-by-event as soon as the upstream emits them. No 16 KiB ReadFull wait.
- **`/admin/limits` endpoint** for live quota inspection — see below.
- **In-memory per-provider stats** — request counts by status class (2xx/429/other), failover-hit counter, and last-event timestamps, surfaced over `/admin/limits`.

## Endpoint: `GET /admin/limits`

Designed for a CLI dashboard. For each provider it returns:

1. **Live counters** (`requests_2xx`, `requests_429`, `requests_other`, `failover_hits`) updated atomically from the request path; resets to zero on proxy restart.
2. **Quota snapshot** — the proxy issues `GET https://api.minimax.io/v1/token_plan/remains` for each key (CN mirror `api.minimaxi.com` if global returns 401/403), parses the `model_remains[]` entry for `model_name == "general"` and exposes both the 5-hour and weekly windows:
   - `pct_remaining` — percent remaining (NOT consumed; MiniMax semantics — note this contradicts the literal field name, see [openclaw #86885](https://github.com/openclaw/openclaw/issues/86885))
   - `total_count` / `remaining_count` / `consumed_count`
   - `reset_in_s` — Unix seconds until reset
   - `window_start_unix` / `window_end_unix` (Unix seconds)
   - `status` — integer state (1 = active, 3 = exhausted)

Sample response:

```json
{
  "uptime_unix": 1784718940,
  "providers": [
    {
      "provider": "minimax-coding-plan",
      "stats": {
        "requests_2xx": 1247,
        "requests_429": 3,
        "requests_other": 0,
        "failover_hits": 3
      },
      "quota": {
        "ok": true,
        "source_url": "https://api.minimax.io/v1/token_plan/remains",
        "five_h":   { "pct_remaining": 81, "total_count": 0, "remaining_count": 0, "consumed_count": 0, "reset_in_s": 13211, "status": 1, "window_start_unix": 1784714400, "window_end_unix": 1784732400 },
        "weekly":   { "pct_remaining": 53, "total_count": 0, "remaining_count": 0, "consumed_count": 0, "reset_in_s": 391460, "status": 1, "window_start_unix": 1784505600, "window_end_unix": 1785110400 }
      }
    },
    { "...": "second provider" }
  ]
}
```

## CLI companion

Pair this with the [`oco`](https://github.com/sst/opencode) wrapper's `limits` subcommand (or use `curl + jq` directly):

```
$ oco limits
minimax-proxy @ 127.0.0.1:8443
  fetched: 2026-07-22T11:22:47Z

provider minimax-coding-plan
  stats:  2xx=1247  429=3  failover=3  other=0
  source: https://api.minimax.io/v1/token_plan/remains
  5h:     81% remaining  ████████████████░░░░  reset in 3h 39m
  week:   53% remaining  ██████████░░░░░░░░░░  reset in 4d 12h

provider minimax
  stats:  2xx=0  429=0  failover=0  other=0
  source: https://api.minimax.io/v1/token_plan/remains
  5h:    100% remaining  ████████████████████  reset in 3h 39m
  week:   96% remaining  ███████████████████░  reset in 4d 12h
```

Bars colorize green / yellow / red at the 10 % / 3 % thresholds. Pass `--json` for the raw endpoint output, or `--name <substring>` to filter to a single provider.

## Install

Requires Go 1.23+.

```sh
git clone https://github.com/thegalkin/minimax-proxy.git
cd minimax-proxy
go build -trimpath -ldflags="-s -w" -o minimax-proxy .
cp .env.example .env
$EDITOR .env                  # paste both keys (see below)
chmod 600 .env                # required — proxy refuses startup if mode is wider

# Run directly
./minimax-proxy

# Or under systemd (user unit)
cp minimax-proxy.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now minimax-proxy
curl -s http://127.0.0.1:8443/healthz
```

`.env` keys:

| Variable | Required | Default | Effect |
| --- | --- | --- | --- |
| `MINIMAX_CODING_PLAN_KEY` | yes | — | Primary key, tried first |
| `MINIMAX_KEY` | yes | — | Fallback key, tried on 429 + `rate_limit_error`. Must differ from the primary — the proxy refuses to start if both keys are identical |
| `LISTEN_ADDR` | no | `127.0.0.1:8443` | Bind address |

## Where the keys come from

The keys live in **two** places and they must stay in sync:

1. **Source of truth** — `~/.local/share/opencode/auth.json`, under the
   `"minimax"` and `"minimax-coding-plan"` keys. opencode stores them there
   when you sign in to the platform; the `fetch-quotas.sh` cron job reads
   them directly. They survive across reinstalls because opencode owns
   this file.
2. **Live config for the proxy** —
   `~/.config/opencode/quota-state/secrets/minimax-proxy.env`, mode 0600.
   The systemd unit points `EnvironmentFile=` at this path. The
   `secrets/` directory is excluded from Syncthing via `.stignore`, so the
   keys never leak to other machines.

The repo ships a symlink `~/github/minimax-proxy/.env` → the secrets file,
so running `./minimax-proxy` directly from the repo also works (it uses
`ENV_FILE=$DIR/.env` from `run.sh`).

**To refresh the keys** (e.g. opencode was re-authenticated and got a new
`auth.json`):

```sh
j=$(jq -r '.["minimax-coding-plan"].key' ~/.local/share/opencode/auth.json)
k=$(jq -r '.minimax.key'                ~/.local/share/opencode/auth.json)
install -m 600 /dev/null ~/.config/opencode/quota-state/secrets/minimax-proxy.env
{
  printf 'MINIMAX_CODING_PLAN_KEY=%s\nMINIMAX_KEY=%s\n' "$j" "$k"
  [ -n "${LISTEN_ADDR:-}" ] && printf 'LISTEN_ADDR=%s\n' "$LISTEN_ADDR"
} > ~/.config/opencode/quota-state/secrets/minimax-proxy.env
systemctl --user restart minimax-proxy
```

## Configure opencode

Replace the `minimax` provider's `baseURL` so opencode talks to the proxy instead of the upstream directly:

```jsonc
// ~/.config/opencode/opencode.json
"provider": {
  "minimax": {
    "npm": "@ai-sdk/anthropic",
    "name": "MiniMax (via proxy)",
    "options": { "baseURL": "http://127.0.0.1:8443" },
    "models": { /* unchanged */ }
  }
}
```

The provider ID is `minimax` — the proxy is transparent about which key it uses, so no per-key wiring is needed.

## Sounds (optional)

`sounds/{en,ru}/limit_{5h,weekly}.wav` are pre-generated placeholder voice announcements (espeak-ng synthesised, mono 22050 Hz). They are NOT wired into the proxy itself — the proxy is silent.

To opt into spoken alerts when the upstream quota drops below a threshold, run a sidecar that polls `/admin/limits` and plays these files. Example:

```sh
# 5-minute quota watcher: announce 5h window when it drops below 10 %
while true; do
  pct=$(curl -sf http://127.0.0.1:8443/admin/limits \
        | jq '.providers[0].quota.five_h.pct_remaining')
  if [ "$pct" -lt 10 ]; then
    ffplay -autoexit -nodisp -loglevel error sounds/en/limit_5h.wav
  fi
  sleep 300
done
```

Swap `sounds/en/` for `sounds/ru/` if you prefer the Russian voice, or record your own and overwrite the files in place.

## Why this exists

opencode's `fallback_models` chain doesn't fire on a `RateLimitError` from the MiniMax coding-plan endpoint — the runtime retries the same provider until its own `retry.maxDelayMs` ceiling aborts, and you see the `Retry Error` toast. This proxy sits between opencode and `api.minimax.io`, retries with a second key on HTTP 429 + `type:"rate_limit_error"`, and only returns the final response once one of the two keys succeeds.

## Upstream timeouts and failover

The opencode.ai upstream has been observed to hang for many minutes on large
(~1 MB) requests. The proxy used to wait forever — opencode's client retried
with an ever-growing body (the failed turn is re-sent with the error injected
into the system prompt), burning input tokens on every attempt.

The proxy now:

- bounds the wait for upstream **response headers** (`timeout_s` per rule,
  `default_timeout_s` at the top level, built-in default 120 s). SSE body
  streaming itself stays unbounded;
- **fails over to the next key** when an attempt times out or dies on a
  transport error (previously only 401/403/429 triggered failover);
- returns **504** to the client when all keys time out (429 is still used when
  all keys answered 401/403/429);
- **cancels the upstream request when the client disconnects** — upstream
  requests are created with the client's context, so opencode abandoning a
  request no longer leaves zombie upstream connections.

## Built-in routing table

With no `~/.config/llm-proxy/config.yaml` (or with no `rules:` in it) the
proxy routes out of the box:

| request model | upstream | model rewrite |
| --- | --- | --- |
| `GO/<name>` | opencode-go | `<name>` (e.g. `GO/deepseek-v4-flash` → `deepseek-v4-flash`) |
| `opencode-zen/zen/<name>` | opencode-zen | `<name>` (the `zen/` label is routing-only; the API serves bare names) |
| `MiniMax-*` (on `/v1/messages`) | minimax | canonical `MiniMax-<name>` |
| anything else | default upstream (opencode-go) | — |

Wildcard `to:` targets like `opencode-go/*` preserve the captured model name
(the star capture previously rewrote the model to a literal `*`).

## Safety

- `.env` is `chmod 600`; the proxy does not log its contents.
- By default binds to `127.0.0.1` only — the bundled systemd unit sets `LISTEN_ADDR=127.0.0.1:8443`.
- Upstream responses are passed through unchanged — status, headers, body.
- The `/admin/*` endpoints are unauthenticated; if you widen `LISTEN_ADDR` beyond loopback, gate them at the network layer.

## Development

Pre-commit expectations:

- `go build .` exits 0 with no warnings.
- `rg -n 'sk-cp-[A-Za-z0-9_-]{20,}' main.go .env.example` returns nothing.
- `rg -n 'thegalkin|/var/home/thegalkin' main.go` returns nothing.
- `ls sounds/*/limit_*.wav | wc -l` equals 4 (en/limit_5h, en/limit_weekly, ru/limit_5h, ru/limit_weekly).

## License

MIT.
