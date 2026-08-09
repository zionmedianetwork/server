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

**`http.go` — the server.** `NewHTTP(cfg, logger)` builds an Echo v4 instance with the middleware stack fixed at construction time (RemoveTrailingSlash, RequestID, BodyLimit, Recover, request logger, CORS) and returns an unexported `*httpServer`. The consumer's flow is:

1. `NewHTTP(...)` — pass `nil` config to load from env.
2. `s.Echo()` — the only way to register application routes; there is no route-registration API on `httpServer` itself.
3. `s.Run() error` — starts `StartH2CServer` in a goroutine (HTTP/2 cleartext, no TLS — TLS termination is expected upstream), blocks until SIGINT **or SIGTERM**, then drains within `HTTP_SHUTDOWN_TIMEOUT`.

`Run` never terminates the calling process and never logs on the caller's behalf — the returned error is its only report, so handle it:

```go
if err := s.Run(); err != nil { log.Errorw("server", "error", err); os.Exit(1) }
```

It returns nil on a clean drain, and non-nil on a failed bind or a drain that overran the grace period. Discarding the result compiles (it did before `Run` returned anything) but means a failed bind is silent.

Internally `Run` is a thin wrapper over `run(ctx, stop) error`, which is the testable seam — tests drive the whole shutdown path with a plain context instead of real signals. `stop()` is called when the drain begins, so a *second* signal takes the default action and aborts a stuck drain.

**Probes.** `/healthz` + `/v1/healthz` are liveness (static 200). `/readyz` + `/v1/readyz` are readiness: they run checks registered via `RegisterReadinessCheck(name, func(ctx) error)`, concurrently, bounded by `HTTP_READINESS_TIMEOUT`, and answer 503 if any fails. All four paths are skipped by the request logger. Readiness bodies name the failing check but include its error text only when `HTTP_DEBUG` is set — the cause always goes to the log at warn instead (see `readiness.go`, which logs transitions plus one repeat per minute rather than per probe).

**`logger.go` — the logging contract.** `NewHTTP` takes a `server.Logger`: a three-method interface (`Infow`, `Warnw`, `Errorw`). A `log/slog` adapter is three one-line methods; `logam.Logger` satisfies it as-is, so pre-existing callers compile untouched (enforced by `logam_compat_test.go`). **Do not widen this interface** — in particular never add `Fatal`, which is how defect C1 killed the caller's process mid-drain; keeping it unnameable is what makes that unrepresentable. This package never constructs a logger.

Request logs carry the RequestID middleware's id. The level rule is `Errorw` when `v.Error != nil || status >= 500`, else `Infow` — note the sharp edge: a handler *returning* `echo.NewHTTPError(400, ...)` logs at **error**, because the error is non-nil regardless of status.

**`config.go` — env configuration.** `HttpConfig` is populated by `envconfig` with prefix `http`. Field names drive the env var names, so watch the spellings:

- `HTTP_BIND_ADDRESS` (default `:8080`)
- `HTTP_MAX_BODY_LIMIT` (default `10M` = 10,000,000 bytes — gommon parses `M` as *decimal*; use `10MiB` for binary). Validated at construction: an unparseable or non-positive value returns an error from `NewHTTP` rather than panicking inside echo's middleware.
- `HTTP_ALLLOWED_ORIGINS` — **three L's**, the `AlllowedOrigins` field is misspelled and the env var inherits it (default `*`)
- `HTTP_STATIC_PATH`
- `HTTP_DEBUG` (default **false**) — when true, echo copies the handler's `err.Error()` into the response body *and* indents all JSON. Off by default because those errors carry SQL text and DSN credentials.
- `HTTP_REAL_IP_SOURCE` — `peer` (default, ignores forwarding headers) or `xff`. Determines `c.RealIP()` and therefore the `remote_ip` in every log line.
- `HTTP_TRUSTED_PROXIES` — comma-separated CIDRs, only meaningful with `xff`. Empty means loopback + private ranges are trusted; **set, the list is exhaustive** and nothing else is. Setting it while the source is `peer` is a startup error, not a no-op.
- `HTTP_READTIMEOUT` / `HTTP_WRITETIMEOUT` — no `split_words` tag on these two, so they are *not* `HTTP_READ_TIMEOUT`
- `HTTP_SHUTDOWN_TIMEOUT` — drain grace period (default 10s). Deliberately `split_words`, unlike the two above; don't copy their style for new fields.
- `HTTP_REQUEST_TIMEOUT` (default `0`, off) — cancels the request context and answers 503. Must be **strictly below** `HTTP_WRITETIMEOUT` or `NewHTTP` refuses to start: above it, the transport cuts the connection before the middleware can write a status. Only helps handlers that respect `c.Request().Context()`; one that ignores it runs to completion. A handler that observes cancellation but returns some *other* error yields 500, not 503.
- `HTTP_TIMEOUT_EXEMPT_PATHS` — comma-separated route patterns exempt from the above (streaming, uploads). Exact match on the route pattern, not a prefix.
- `HTTP_READINESS_TIMEOUT` (default `2s`) — bounds one readiness probe across all checks.

Defaults for the timeouts and static path are applied in `NewHttpConfig` after `envconfig.Process`, not via struct tags. `shutdownTimeout()` additionally treats a non-positive value as unset at the point of use, because a hand-built `HttpConfig` would otherwise get a zero grace period and sever every in-flight request.

**`response.go` — response envelope.** `HTTPResponse(c, data)` is the intended way to write JSON. It type-switches on the payload: `PostConfirmation` → 201 bare, `PatchConfirmation`/`Confirmation` → 200 bare, anything else → 200 wrapped as `{"data": ...}`. Adding a new confirmation type means adding a case here, otherwise it gets silently wrapped in `data`.

## Known rough edges (present in code, fix only if asked)

- **`HTTPResponse` drops a 201 when handed a pointer.** The type switch matches values only, so `HTTPResponse(c, &PostConfirmation{...})` falls through to `default` and returns 200 wrapped in `data`. Characterised by `TestHTTPResponseCharacterizesPointerConfirmations`, deliberately not fixed.
- `cfg.StaticPath` is computed but never used — the static route is hardcoded as `e.Static("/static", "static")`, resolved against the process working directory.
- CORS defaults to `*` with all mutating methods, and `MaxAge` is unset so browsers re-preflight every request.
- No TLS path (H2C only), no websocket, no security headers, no rate limiting, no gzip.
- `httpServer` is returned unexported, so consumers cannot name the type; the middleware stack is fixed at construction.
- Echo's `⇨ http server started on ...` line bypasses the injected logger — `HideBanner` is set, `HidePort` is not.

The full severity-ranked review lives on the local `docs/http-server-assessment` branch (`ASSESSMENT.md`), which is deliberately **never pushed**. It tracks which findings are resolved and which remain.
