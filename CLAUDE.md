# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go **library** (`package server`, module `github.com/zionmedianetwork/server`) consumed by other zionmedianetwork services. The package itself has no `main`, but `examples/` holds four runnable services that double as documentation — they are in the same module, so CI compiles and lints them and they cannot rot.

It serves HTTP only. Despite the repo's history, there is no websocket implementation and the README no longer claims one.

## Commands

`go.sum` **is** committed and `go.mod` is tidy, so a fresh clone builds directly — no `go mod tidy` step. (It was missing until PR #3; older guidance saying otherwise is stale, and CI now enforces both.)

```bash
go build ./...
go vet ./...
gofmt -l .                             # must print nothing
go test ./...
go test -race ./...                    # several tests are race- and timing-sensitive
go test -run '^TestName$' ./...        # single test
golangci-lint run ./...                # config in .golangci.yml
```

CI (`.github/workflows/ci.yml`) runs all of the above plus `go mod verify`, a tidy check, a guard that `go.sum` stays tracked, and `govulncheck`. Do not "fix" a CI failure by regenerating `go.mod`/`go.sum` — a tidy diff there means something else is wrong.

## Architecture

**`http.go` — the server.** `NewHTTP(cfg, logger)` builds an Echo v4 instance with the middleware stack fixed at construction time and returns an unexported `*httpServer`. Outermost first, which is registration order:

1. `Pre`: RemoveTrailingSlash, RequestID
2. security headers (unless `HTTP_DISABLE_SECURITY_HEADERS`)
3. CORS
4. BodyLimit, request logger, Recover
5. rate limiter (only with `HTTP_RATE_LIMIT`)
6. gzip (only with `HTTP_GZIP`)
7. request timeout (only with `HTTP_REQUEST_TIMEOUT`)

Three placements are load-bearing and are commented as such in `NewHTTP`. **CORS is outside every middleware that can refuse a request** — a cross-origin 413/429/500 without `Access-Control-Allow-Origin` is withheld from the calling script entirely, so a rejected browser upload looked like a CORS bug rather than a too-large file. The price is that preflights, which CORS answers itself, no longer reach the access log. **The rate limiter is inside the access log**, so every 429 is recorded. **The request logger is outside `Recover`**, which is finding M2: echo's request logger builds its entry after `next(c)` returns and does not defer it, so with `Recover` outside (as it was through PR #1) a panic unwound past the logger and a 500 got no access log line at all. Do not swap these two back — `TestPanicIsWrittenToTheAccessLog` fails if you do.

The consumer's flow is:

1. `NewHTTP(...)` — pass `nil` config to load from env.
2. `s.Echo()` — the only way to register application routes; there is no route-registration API on `httpServer` itself.
3. `s.Run() error` — starts `StartH2CServer` in a goroutine (HTTP/2 cleartext, no TLS — TLS termination is expected upstream), blocks until SIGINT **or SIGTERM**, then drains within `HTTP_SHUTDOWN_TIMEOUT`.

`s.Addr() (net.Addr, bool)` is the fourth exported method and the only accessor besides `Echo()`: the address actually bound, which is the one thing `HTTP_BIND_ADDRESS=:0` cannot tell you. `false` until `Run` binds, which is why it is comma-ok — a bare `net.Addr` return would be the nil interface that panics on `.String()` at the caller. It delegates to `echo.ListenerAddr()`, so it takes the startup lock and is safe to poll from another goroutine, and it also sees a listener assigned directly to `e.Listener` (which is how `listenTestServer` starts servers). It keeps answering after a drain — `Shutdown` closes the listener without clearing it — so it is "what did this bind", not "is this serving".

`Run` never terminates the calling process and never logs on the caller's behalf — the returned error is its only report, so handle it:

```go
if err := s.Run(); err != nil { log.Errorw("server", "error", err); os.Exit(1) }
```

It returns nil on a clean drain, and non-nil on a failed bind or a drain that overran the grace period. Discarding the result compiles (it did before `Run` returned anything) but means a failed bind is silent.

Internally `Run` is a thin wrapper over `run(ctx, stop) error`, which is the testable seam — tests drive the whole shutdown path with a plain context instead of real signals. `stop()` is called when the drain begins, so a *second* signal takes the default action and aborts a stuck drain.

**Probes.** `/healthz` + `/v1/healthz` are liveness (static 200). `/readyz` + `/v1/readyz` are readiness: they run checks registered via `RegisterReadinessCheck(name, func(ctx) error)`, concurrently, bounded by `HTTP_READINESS_TIMEOUT`, and answer 503 if any fails. All four paths are skipped by the request logger. Readiness bodies name the failing check but include its error text only when `HTTP_DEBUG` is set — the cause always goes to the log at warn instead (see `readiness.go`, which logs transitions plus one repeat per minute rather than per probe).

**`logger.go` — the logging contract.** `NewHTTP` takes a `server.Logger`: a three-method interface (`Infow`, `Warnw`, `Errorw`). A `log/slog` adapter is three one-line methods; `logam.Logger` satisfies it as-is, so pre-existing callers compile untouched (enforced by `logam_compat_test.go`). **Do not widen this interface** — in particular never add `Fatal`, which is how defect C1 killed the caller's process mid-drain; keeping it unnameable is what makes that unrepresentable. This package never constructs a logger.

A nil logger is a **startup error**, checked first in `NewHTTP` before the config is even read, so the argument the caller got wrong is the one named. Do not replace that with a no-op default: the package writes nothing on the caller's behalf, and a silent stand-in would mean no access log and no record of a failing readiness check — whose cause is withheld from the probe body and exists nowhere else — on a service that still looks healthy. The check catches a nil interface; a non-nil interface holding a nil pointer is not detectable here without rejecting implementations whose methods handle a nil receiver, and is left alone.

Request logs carry the RequestID middleware's id. The level rule is `Errorw` when `v.Error != nil || status >= 500`, else `Infow` — note the sharp edge: a handler *returning* `echo.NewHTTPError(400, ...)` logs at **error**, because the error is non-nil regardless of status.

A recovered panic logs at error via the *status* half of that rule and carries **no `error` field**: `Recover` runs inside the logger, calls `c.Error` itself and returns nil, so the logger sees a nil error and a 500. The panic value and stack are on stderr only (`[PANIC RECOVER]`, echo's own logger). A 500 with no `error` field means "look at stderr".

**`config.go` — env configuration.** `HttpConfig` is populated by `envconfig` with prefix `http`. Field names drive the env var names, so watch the spellings:

- `HTTP_BIND_ADDRESS` (default `:8080`)
- `HTTP_MAX_BODY_LIMIT` (default `10M` = 10,000,000 bytes — gommon parses `M` as *decimal*; use `10MiB` for binary). Validated at construction: an unparseable or non-positive value returns an error from `NewHTTP` rather than panicking inside echo's middleware.
- `HTTP_ALLLOWED_ORIGINS` — **three L's**, the `AlllowedOrigins` field is misspelled and the env var inherits it. Default is now **empty = no cross-origin access**; `HTTP_ALLLOWED_ORIGINS=*` restores the old wildcard. Entries must be bare origins (`https://app.example.com`, or `https://*.example.com`) — a trailing slash, a path or a missing scheme is a startup error, as is `*` mixed with named origins. `Access-Control-Allow-Credentials` is never sent and has no knob.
- `HTTP_ALLOWED_HEADERS` — **one L**, and a different variable from the one above. Preflight request-header allowlist, default `Accept,Authorization,Content-Type,X-Request-Id`. `*` restores echo's old reflect-the-request behaviour.
- `HTTP_CORS_MAX_AGE` (default `1h`, **applied via the `default:"1h"` struct tag**, not in `NewHttpConfig`, so that `0` stays reachable and keeps meaning "send no header" — the previous behaviour). Sub-second or negative values are startup errors: the header is whole seconds and would truncate to off. Chrome caps at 2h and Firefox at 24h, so bigger is not better.
- `HTTP_DISABLE_SECURITY_HEADERS` (default false, i.e. headers **on**) — nosniff, `X-Frame-Options: SAMEORIGIN`, `Referrer-Policy: strict-origin-when-cross-origin`, `X-XSS-Protection: 0` (deliberate: the legacy auditor was itself a vulnerability). Negatively named on purpose so a hand-built `HttpConfig`'s zero value is the safe one.
- `HTTP_HSTS_MAX_AGE` (default `0`, off) and `HTTP_HSTS_INCLUDE_SUBDOMAINS` (default false). **Do not make HSTS default-on**: this server is h2c with no TLS, echo only emits the header when the request was TLS or carried `X-Forwarded-Proto: https`, and the instruction outlives the response by `max-age`. A max age set while headers are disabled, or subdomains set with no max age, is a startup error.
- `HTTP_RATE_LIMIT` (req/s per client IP, default `0` = off) and `HTTP_RATE_LIMIT_BURST` (defaults to one second's worth, rounded up, never zero). Off by default: per-process limiter, in-memory store keyed on `c.RealIP()`, so it multiplies by replica count and inherits `HTTP_REAL_IP_SOURCE`. Probe paths are never limited — a 429 on `/readyz` or `/healthz` would turn load into an outage. A burst with no rate is a startup error.
- `HTTP_GZIP` (default false) — 1 KiB minimum, skips probe paths **and every route in `HTTP_TIMEOUT_EXEMPT_PATHS`** (compression buffers; that list is where streaming/download routes are already named).
- `HTTP_STATIC_PATH`
- `HTTP_DEBUG` (default **false**) — when true, echo copies the handler's `err.Error()` into the response body *and* indents all JSON. Off by default because those errors carry SQL text and DSN credentials.
- `HTTP_REAL_IP_SOURCE` — `peer` (default, ignores forwarding headers) or `xff`. Determines `c.RealIP()` and therefore the `remote_ip` in every log line.
- `HTTP_TRUSTED_PROXIES` — comma-separated CIDRs, only meaningful with `xff`. Empty means loopback + private ranges are trusted; **set, the list is exhaustive** and nothing else is. Setting it while the source is `peer` is a startup error, not a no-op.
- `HTTP_READTIMEOUT` / `HTTP_WRITETIMEOUT` — no `split_words` tag on these two, so they are *not* `HTTP_READ_TIMEOUT`
- `HTTP_SHUTDOWN_TIMEOUT` — drain grace period (default 10s). Deliberately `split_words`, unlike the two above; don't copy their style for new fields.
- `HTTP_REQUEST_TIMEOUT` (default `0`, off) — cancels the request context and answers 503. Must be **strictly below** `HTTP_WRITETIMEOUT` or `NewHTTP` refuses to start: above it, the transport cuts the connection before the middleware can write a status. Only helps handlers that respect `c.Request().Context()`; one that ignores it runs to completion. A handler that observes cancellation but returns some *other* error yields 500, not 503.
- `HTTP_TIMEOUT_EXEMPT_PATHS` — comma-separated route patterns exempt from the above (streaming, uploads). Exact match on the route pattern, not a prefix.
- `HTTP_READINESS_TIMEOUT` (default `2s`) — bounds one readiness probe across all checks.

Defaults for the timeouts, the static path and the CORS header allowlist are applied in `NewHttpConfig` after `envconfig.Process`, not via struct tags. The two exceptions are `HTTP_BIND_ADDRESS` and `HTTP_CORS_MAX_AGE`, which carry `default:` tags — the latter because zero has to remain reachable as "off"; `TestNewHttpConfigCorsSettings` fails if the tag and `defaultCorsMaxAge` drift apart. `shutdownTimeout()` additionally treats a non-positive value as unset at the point of use, because a hand-built `HttpConfig` would otherwise get a zero grace period and sever every in-flight request.

**`response.go` — response envelope.** `HTTPResponse(c, data)` is the intended way to write JSON. It type-switches on the payload: `PostConfirmation` → 201 bare, `PatchConfirmation`/`Confirmation` → 200 bare, anything else → 200 wrapped as `{"data": ...}`. **Pointers behave identically to values** — the switch names each type twice, so `&PostConfirmation{...}` is the same 201 as the value form. A *nil* pointer is not a confirmation and takes the envelope (`200 {"data":null}`), which is what keeps a typed nil from being dereferenced in a response path.

Adding a confirmation type means adding **both** its cases, value and pointer; one without the other is exactly the defect (R7) this switch was fixed for, and `TestHTTPResponsePointerConfirmationsMatchValues` asserts one expectation per type against both forms. Do not "simplify" the six cases into an unexported marker method asserted as an interface: a method is promoted by embedding, so `PatchConfirmation` would inherit `PostConfirmation`'s 201 and a consumer's `struct{ server.PostConfirmation; ... }` would silently switch from 200-wrapped to 201-bare. A type switch promotes nothing.

`Singular` was removed here — it was exported, unused anywhere in the repo, and an identical `map[string]interface{}` to `ResponsePayload`. Consumers naming it get a compile error and a one-token rename.

## Known rough edges (present in code, fix only if asked)

- `cfg.StaticPath` is computed but never used — the static route is hardcoded as `e.Static("/static", "static")`, resolved against the process working directory.
- No TLS path (H2C only) and no websocket.
- **A `413` from a declared `Content-Length` never reaches the access log.** `BodyLimit` is registered above the request logger and returns `echo.ErrStatusRequestEntityTooLarge` *before* calling `next`, so nothing below it — logger included — runs; the client gets a correct 413 with CORS and security headers and the log gets nothing. An oversize body with no declared length is logged, because the limit is then hit inside the handler's read and comes back as a returned error. Unchanged by the M2 reorder (`BodyLimit` was outside the logger both before and after) and deliberately not fixed here: the fix is to register the logger between CORS and `BodyLimit`, which changes what the logger wraps for every request, not just the panicking ones, and is a larger reordering than M2 called for.
- A rate limiter keyed on `c.RealIP()` is only as good as `HTTP_REAL_IP_SOURCE`; the in-memory store is per process.
- `httpServer` is returned unexported, so consumers cannot name the type; the middleware stack is fixed at construction.
- Echo's `⇨ http server started on ...` line bypasses the injected logger — `HideBanner` is set, `HidePort` is not.

The full severity-ranked review lives on the local `docs/http-server-assessment` branch (`ASSESSMENT.md`), which is deliberately **never pushed**. It tracks which findings are resolved and which remain.
