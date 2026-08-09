# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go **library** (`package server`, module `github.com/zionmedianetwork/server`) consumed by other zionmedianetwork services. There is no `main` package and no tests yet — nothing here runs on its own. Per README it is meant to provide "http and websocket servers"; only the HTTP side exists today.

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

`/healthz` and `/v1/healthz` are registered internally and are skipped by the request logger, so probe traffic stays out of the log.

Logging is the `github.com/zionmedianetwork/logam` interface (zap-backed); callers supply it — this package never constructs one. Request logs are emitted through it as structured fields (`Errorw` on error or 5xx, `Infow` otherwise), including the RequestID middleware's id.

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

Defaults for the timeouts and static path are applied in `NewHttpConfig` after `envconfig.Process`, not via struct tags. `shutdownTimeout()` additionally treats a non-positive value as unset at the point of use, because a hand-built `HttpConfig` would otherwise get a zero grace period and sever every in-flight request.

**`response.go` — response envelope.** `HTTPResponse(c, data)` is the intended way to write JSON. It type-switches on the payload: `PostConfirmation` → 201 bare, `PatchConfirmation`/`Confirmation` → 200 bare, anything else → 200 wrapped as `{"data": ...}`. Adding a new confirmation type means adding a case here, otherwise it gets silently wrapped in `data`.

## Known rough edges (present in code, fix only if asked)

- `e.Debug = true` is hardcoded in `NewHTTP` regardless of environment.
- `cfg.StaticPath` is computed but never used — the static route is hardcoded as `e.Static("/static", "static")`.
- No websocket server despite the README.
