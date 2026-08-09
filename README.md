# server

`github.com/zionmedianetwork/server` is a small, opinionated wrapper around
[Echo v4](https://echo.labstack.com/) that gives a Go service an HTTP listener
with the parts every service needs already wired: structured access logging
through a logger you inject, a request id, a body limit, panic recovery, CORS,
liveness and readiness endpoints, an optional request timeout, and a graceful
shutdown that drains in-flight requests on `SIGINT`/`SIGTERM`.

The logger is the only thing it asks you for, and it asks for three methods —
`Infow`, `Warnw`, `Errorw` — so `log/slog` from the standard library is enough.
See [Logging](#logging).

It is a library. There is no `main`, nothing runs on its own, and everything it
does is decided by a consumer's call to `NewHTTP` and by `HTTP_*` environment
variables.

## What it is not

- **Not a TLS server.** It serves **H2C — HTTP/2 cleartext** — via Echo's
  `StartH2CServer`. There is no certificate handling and no listener option to
  add one. **TLS termination is expected upstream**, in an ingress controller,
  a service mesh sidecar or a cloud load balancer. Exposing this process
  directly to clients puts every request, response, credential and cookie on
  the wire in plaintext. That is a deployment requirement, not a footnote: if
  nothing terminates TLS in front of it, this server is not safe to expose.
- **Not a websocket server.** The package name and the previous README claimed
  "http and websocket servers". There has never been a websocket
  implementation here. Nothing in this repository upgrades a connection.
- **Not a framework.** It registers no application routes and imposes no
  handler signature beyond Echo's. You get an `*echo.Echo` and use it.

## Install

```bash
go get github.com/zionmedianetwork/server
```

Requires Go 1.23.3 or newer (the `go` directive in `go.mod`).

## Quick start

```go
package main

import (
	"log/slog"
	"os"

	"github.com/labstack/echo/v4"
	"github.com/zionmedianetwork/server"
)

// The logger this package wants is three methods. slog takes the same
// alternating key/value arguments, so each one forwards them untouched.
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Infow(msg string, kv ...interface{})  { s.l.Info(msg, kv...) }
func (s slogLogger) Warnw(msg string, kv ...interface{})  { s.l.Warn(msg, kv...) }
func (s slogLogger) Errorw(msg string, kv ...interface{}) { s.l.Error(msg, kv...) }

func main() {
	log := slogLogger{l: slog.New(slog.NewJSONHandler(os.Stdout, nil))}

	// nil config means "read HTTP_* from the environment".
	s, err := server.NewHTTP(nil, log)
	if err != nil {
		log.Errorw("build http server", "error", err)
		os.Exit(1)
	}

	s.Echo().GET("/v1/videos/:id", func(c echo.Context) error {
		return server.HTTPResponse(c, map[string]string{"id": c.Param("id")})
	})

	// Run blocks until SIGINT/SIGTERM, drains, and returns. Handle the error:
	// it is the only report a failed bind will ever make.
	if err := s.Run(); err != nil {
		log.Errorw("http server stopped", "error", err)
		os.Exit(1)
	}
}
```

Two non-standard-library imports, and that is the whole dependency list: this
package, and Echo for the handler signature. The logging is `log/slog`, so it
costs nothing to add. If your service already has a logger — including
`github.com/zionmedianetwork/logam`, whose `Logger` satisfies the interface as
it is, with no adapter — pass that instead and delete the three methods. See
[Logging](#logging).

Four runnable, lint-clean programs live in [`examples/`](examples/) — start
there rather than with the snippet above.

## The consumer flow

Three calls, in this order.

### 1. `NewHTTP(cfg *HttpConfig, log Logger) (*httpServer, error)`

Pass `nil` for `cfg` to load the configuration from the environment
(equivalent to calling `NewHttpConfig()` yourself and passing the result).

`log` is a `server.Logger`: `Infow`, `Warnw`, `Errorw`, and nothing else. Any
value with those three methods is accepted, so this parameter does not oblige
you to adopt a logging library — a `log/slog` adapter is three lines, and a
`logam.Logger` satisfies it directly. There is no default: the package logs
through the value it is given, on every request and every readiness probe, and
installs no no-op in its place. See [Logging](#logging).

Everything the configuration decides is resolved and validated here, before
anything is wired up, so a misconfigured server is reported by this call and
never half-built. `NewHTTP` returns an error — it does not panic and does not
exit — for:

| Cause | Example message fragment |
| --- | --- |
| `HTTP_MAX_BODY_LIMIT` unparseable or non-positive | `invalid max body limit "10 megs"` |
| `HTTP_REAL_IP_SOURCE` not `peer` or `xff` | `invalid real ip source "XFF"` |
| An entry in `HTTP_TRUSTED_PROXIES` that is not CIDR | `invalid trusted proxy "10.1.0.7"` |
| `HTTP_TRUSTED_PROXIES` set while the source is `peer` | `set HTTP_REAL_IP_SOURCE=xff or drop HTTP_TRUSTED_PROXIES` |
| `HTTP_REQUEST_TIMEOUT` negative | `want a positive duration, or zero to disable it` |
| `HTTP_REQUEST_TIMEOUT` at or above `HTTP_WRITETIMEOUT` | `must be below the write timeout 15s` |
| An entry in `HTTP_TIMEOUT_EXEMPT_PATHS` without a leading `/` | `want a route pattern beginning with "/"` |

The returned type is unexported. You cannot name it in a variable
declaration, a struct field or a function signature — hold it in a `:=`
binding, or define your own narrow interface if you need to pass it around.
See [Known limitations](#known-limitations).

### 2. `s.Echo() *echo.Echo`

The only route-registration API. There is none on the server itself.

```go
e := s.Echo()
e.GET("/v1/videos/:id", getVideo)
e.POST("/v1/videos", createVideo)
```

The built-in middleware stack is applied in this order and is fixed at
construction:

1. `Pre`: `RemoveTrailingSlash`, `RequestID`
2. `Use`: `BodyLimit`, `Recover`, request logger, CORS
3. `Use`: request timeout — only when `HTTP_REQUEST_TIMEOUT` is set

You may append your own with `e.Use(...)` or `e.Pre(...)`, but you cannot
reorder or remove the built-ins, and anything you append with `Use` runs
*inside* the request timeout.

Register every route before calling `Run()`: Echo's router is not guarded
against concurrent registration, so adding a route while the server is serving
races with the requests it is routing. `RegisterReadinessCheck` is the
exception — its registry has its own lock and is explicitly safe to call while
serving.

### 3. `s.Run() error`

`Run` installs handlers for `SIGINT` and `SIGTERM` before anything starts
listening, serves, and blocks. On a signal it stops accepting connections and
drains in-flight requests within `HTTP_SHUTDOWN_TIMEOUT`.

| Situation | Result |
| --- | --- |
| Clean drain after a signal | `nil` |
| Failed bind (port in use, malformed address) | non-nil, naming the address |
| Drain overran the grace period | non-nil, wrapping `context.DeadlineExceeded` |

Three properties are worth stating explicitly, because each has bitten someone:

- **`Run` never terminates the process.** Deciding that a failure to serve
  warrants a non-zero exit status belongs to the application.
- **`Run` never logs on the caller's behalf.** The returned error is its only
  report. A caller that discards it gets *silence* on a failed bind — the
  process stays up, serves nothing and exits 0.
- **A second signal during a drain takes its default action.** The signal
  handlers are released the moment draining starts, so a `Ctrl-C` on a drain
  that is not progressing aborts it, as everyone expects.

The recommended call site:

```go
if err := s.Run(); err != nil {
	log.Errorw("http server stopped", "error", err)
	os.Exit(1)
}
```

A clean shutdown returns `nil`, so this does not turn an ordinary SIGTERM from
an orchestrator into a failed exit.

## Configuration

`HttpConfig` is populated by [envconfig](https://github.com/kelseyhightower/envconfig)
with the prefix `HTTP`. Durations use Go's `time.ParseDuration` syntax
(`15s`, `2m`, `500ms`); lists are comma-separated; booleans accept
`true`/`false`/`1`/`0`.

| Environment variable | Field | Default | Meaning |
| --- | --- | --- | --- |
| `HTTP_BIND_ADDRESS` | `BindAddress` | `:8080` | Address the listener binds. |
| `HTTP_READTIMEOUT` | `ReadTimeout` | `15s` | `http.Server.ReadTimeout`: the whole request, headers and body, must arrive within it. |
| `HTTP_WRITETIMEOUT` | `WriteTimeout` | `15s` | `http.Server.WriteTimeout`: the connection is closed once it passes, whatever the handler is doing. |
| `HTTP_SHUTDOWN_TIMEOUT` | `ShutdownTimeout` | `10s` | Grace period in-flight requests get after a termination signal. |
| `HTTP_REQUEST_TIMEOUT` | `RequestTimeout` | `0` (off) | Deadline put on the request context. Must be strictly below `HTTP_WRITETIMEOUT`. See [Request timeouts](#request-timeouts). |
| `HTTP_TIMEOUT_EXEMPT_PATHS` | `TimeoutExemptPaths` | empty | Route **patterns** the request timeout skips, e.g. `/videos/:id/stream,/uploads`. Each must start with `/`. |
| `HTTP_READINESS_TIMEOUT` | `ReadinessTimeout` | `2s` | Bound on one readiness probe, covering all checks together. |
| `HTTP_MAX_BODY_LIMIT` | `MaxBodyLimit` | `10M` | Largest accepted request body, in gommon byte notation (`10M`, `512K`, `4MiB`). `M` is **decimal**: `10M` is 10,000,000 bytes; use `10MiB` for binary. |
| `HTTP_ALLLOWED_ORIGINS` | `AlllowedOrigins` | `*` | CORS `Access-Control-Allow-Origin` list. Note the spelling. |
| `HTTP_STATIC_PATH` | `StaticPath` | `/static` | **Currently ignored.** See [Known limitations](#known-limitations). |
| `HTTP_DEBUG` | `Debug` | `false` | Copies handler errors into response bodies, indents all JSON, and adds the cause to readiness reports. |
| `HTTP_REAL_IP_SOURCE` | `RealIPSource` | `peer` | `peer` or `xff`. See [Client IP](#client-ip). |
| `HTTP_TRUSTED_PROXIES` | `TrustedProxies` | empty | Comma-separated CIDRs allowed to relay a client address. Only valid with `xff`. |

Not configurable: the HTTP/2 settings (`MaxConcurrentStreams: 200`,
`MaxReadFrameSize: 1024000`, `IdleTimeout: 10s`), the CORS method list
(`GET, POST, PUT, PATCH, DELETE`), and the middleware stack.

### Two naming gotchas

These are real, they cost people time, and neither can be fixed without a
breaking change to every deployment already setting them.

1. **`HTTP_READTIMEOUT` and `HTTP_WRITETIMEOUT` have no underscore in the
   middle.** Every other multi-word field carries envconfig's
   `split_words:"true"` tag; `ReadTimeout` and `WriteTimeout` do not. So
   `HTTP_READ_TIMEOUT` and `HTTP_WRITE_TIMEOUT` are **silently ignored** and
   you get the 15s defaults:

   ```console
   $ HTTP_WRITE_TIMEOUT=99s ./svc     # ignored
   ... "write_timeout": "15s"
   $ HTTP_WRITETIMEOUT=99s ./svc      # read
   ... "write_timeout": "1m39s"
   ```

   Do not copy this style for new fields — `HTTP_SHUTDOWN_TIMEOUT`,
   `HTTP_REQUEST_TIMEOUT` and `HTTP_READINESS_TIMEOUT` are all split correctly.

2. **`HTTP_ALLLOWED_ORIGINS` has three L's.** The struct field is misspelled
   `AlllowedOrigins`, and envconfig derives the variable name from the field.
   `HTTP_ALLOWED_ORIGINS` does nothing, and the default is `*`, so the mistake
   fails open rather than loudly. The misspelling is also visible in Go code
   for anyone building an `HttpConfig` by hand.

### Building a config by hand

`HttpConfig` and its fields are exported, so a consumer that does not want
environment variables can build one directly. Note that the defaults live in
`NewHttpConfig`, not in struct tags, so a hand-built config gets Go zero values
for anything you leave out. Three of those are defended at the point of use —
an unset body limit, shutdown timeout and readiness timeout each fall back to
the package default rather than doing something absurd — but `ReadTimeout` and
`WriteTimeout` will genuinely be zero, meaning no transport timeout at all.

## Health and readiness

Four routes are registered for you, and all four are excluded from the access
log — probe traffic is high volume and carries no information.

| Path | Kind | Answers |
| --- | --- | --- |
| `/healthz`, `/v1/healthz` | Liveness | Static `200 OK`, body `OK`, `text/plain`. |
| `/readyz`, `/v1/readyz` | Readiness | `200` when every registered check passes, `503` when any fails. |

Liveness is deliberately static. It says "this process is running and
answering" and nothing else: a liveness probe that failed because the database
was unreachable would have an orchestrator restart a pod whose only problem is
somewhere else, turning a dependency outage into a crash loop.

Readiness is what a load balancer should consult.

```go
if err := s.RegisterReadinessCheck("postgres", db.PingContext); err != nil {
	return fmt.Errorf("register readiness check: %w", err)
}
```

`RegisterReadinessCheck(name string, check ReadinessCheck) error` returns an
error for an empty name, a nil check, or a name already taken. It is safe to
call before `Run` and while the server is serving: each probe works from a
snapshot, so a check registered mid-flight is picked up by the next probe.

A `ReadinessCheck` is `func(ctx context.Context) error`. It runs on **every**
probe, so it should be a ping and not a query, and it must respect the context
it is handed — a check that blocks anyway is abandoned at the deadline and
reported as failed, but its goroutine keeps running. Checks run concurrently
(so one slow dependency does not make the others look broken), the whole probe
is bounded by `HTTP_READINESS_TIMEOUT`, and a check that panics is contained
and reported as a failure rather than taking the process down.

Response shape, with no checks registered:

```json
{"status":"ok","checks":[]}
```

With a failing dependency (`503`), checks listed in registration order:

```json
{"status":"fail","checks":[{"name":"postgres","status":"fail"},{"name":"redis","status":"ok"}]}
```

**The cause is not in the body unless `HTTP_DEBUG` is set.** Driver errors name
hosts, ports, databases and accounts — `pq: password authentication failed for
user "zion_admin"` — and readiness is routinely exposed further than the rest
of the API. With `HTTP_DEBUG=true` each failing check gains an `error` field.

The cause is **always** written to the log, in both settings, at **warn**
level, because the probe paths are excluded from the access log and it would
otherwise exist nowhere:

```
WARN  readiness check failed  {"check": "postgres", "error": "pq: dial tcp 10.0.0.7:5432: connect: connection refused", "failing_for": "0s"}
INFO  readiness check recovered  {"check": "postgres", "failing_for": "204.223ms"}
```

Warn rather than error because a dependency going away is expected and usually
self-healing, and this process is doing the right thing by answering 503. To
keep a fleet of pods polled once a second from producing tens of thousands of
identical lines, only transitions are logged, plus a repeat at most once a
minute while a check stays down, plus one line on recovery.

Kubernetes probes should point at `/healthz` for `livenessProbe` and `/readyz`
for `readinessProbe` (and `startupProbe`, if you use one).

## Request timeouts

Off by default, and deliberately so: this package cannot see the routes it will
carry. A blanket timeout is wrong for large downloads and uploads, and for any
streaming or SSE route that holds a request open by design, so a library that
switched one on at upgrade time would convert working routes into 503s without
anybody changing a line of code.

```bash
HTTP_REQUEST_TIMEOUT=10s
HTTP_TIMEOUT_EXEMPT_PATHS=/v1/videos/:id/stream,/v1/uploads
HTTP_WRITETIMEOUT=120s
```

Three things decide whether this works.

**It must be strictly below `HTTP_WRITETIMEOUT`, and `NewHTTP` enforces that.**
`net/http` closes the connection once the write timeout passes, so a request
timeout that is not shorter cancels handlers and never manages to write the
503; the client sees a truncated stream or a reset instead of a status. The
default write timeout is 15s, so the obvious "give requests 30 seconds" is
exactly the setting that cannot work:

```
build http server: request timeout 30s must be below the write timeout 15s, ...
```

**Exemptions name route patterns, not URLs.** `/videos/:id/stream`, not
`/videos/42/stream`; the package's own static route is `/static*`. An entry
without a leading `/` is refused at startup, because it could never match and
would sit there looking like an exemption that works.

**It only bounds handlers that cooperate.** The timeout is implemented by
putting a deadline on `c.Request().Context()`. A handler that selects on that
context, or passes it into its database driver and HTTP clients, is unblocked
at the deadline; returning the context's error (or an error wrapping it with
`%w`) is what the middleware turns into `503 Service Unavailable`. A handler
that ignores the context runs to completion exactly as it would with no timeout
configured, holding its goroutine and its connection — and a handler that
notices cancellation but returns some unrelated error will be answered with
whatever that error maps to, usually 500. Propagating the request context into
every blocking call is what makes the timeout real. See
[`examples/streaming`](examples/streaming) for all three cases side by side.

Note also that exempting a route from the request timeout is not enough on its
own: the write timeout still applies to it, so a long stream needs
`HTTP_WRITETIMEOUT` raised as well or the transport cuts it off regardless.

## Client IP

`HTTP_REAL_IP_SOURCE` decides where `c.RealIP()` reads the client address from,
which is also the `remote_ip` in every access log line and the basis for
anything you build on top (allowlists, audit trails, rate limiting).

| Value | Behaviour |
| --- | --- |
| `peer` (default) | The address that opened the connection. `X-Forwarded-For` and `X-Real-IP` are ignored entirely. |
| `xff` | `X-Forwarded-For`, walked from the right, stopping at the first hop that is not a trusted proxy. `X-Real-IP` is still ignored. |

The default is `peer` because forwarding headers are client-supplied data: with
nothing trusted in front, any caller can claim any address it likes. Echo's own
default (no `IPExtractor` set) believes those headers from anyone; this package
sets an extractor explicitly in every case so that never applies.

`HTTP_TRUSTED_PROXIES` is a comma-separated CIDR list and is only meaningful
with `xff`:

- **Empty**: loopback and the private ranges are trusted, which covers the
  usual sidecar and cluster-internal ingress. Link-local is never trusted.
- **Set**: the list is **exhaustive, not additive**. Naming the load balancer
  says where the proxies are, so the implicit trust in loopback and every
  private address stands down. This surprises people — after setting
  `HTTP_TRUSTED_PROXIES=10.4.0.0/16`, a request relayed from `127.0.0.1` is no
  longer believed.

Setting `HTTP_TRUSTED_PROXIES` while the source is still `peer` is a startup
error, not a no-op: the ranges would never be consulted, and a silent no-op
leaves an operator believing a proxy is trusted. `xff` is also only safe when
the proxy in front **overwrites** rather than appends to an incoming
`X-Forwarded-For`. See [`examples/behind-proxy`](examples/behind-proxy).

## Responses

`HTTPResponse(c echo.Context, data interface{}) error` is the intended way to
write JSON. It type-switches on the payload:

| Payload | Status | Body |
| --- | --- | --- |
| `server.PostConfirmation` | `201` | the value, unwrapped |
| `server.PatchConfirmation` | `200` | the value, unwrapped |
| `server.Confirmation` | `200` | the value, unwrapped |
| anything else | `200` | `{"data": <payload>}` |

```go
// 200 {"data":{"id":"42","title":"Episode 42"}}
return server.HTTPResponse(c, video{ID: "42", Title: "Episode 42"})

// 201 {"resource":"video","message":"created","id":"42"}
return server.HTTPResponse(c, server.PostConfirmation{
	Resource: "video", Message: "created", ID: "42",
})
```

Also exported: `server.Singular` and `server.ResponsePayload`, both
`map[string]interface{}` aliases for building ad-hoc payloads. Adding a new
confirmation type means adding a case to the switch, otherwise it is silently
wrapped in `data`.

### Known issue: pointers lose their status code

The type switch matches **value types only**. Passing a pointer falls through
to the `default` branch, so this:

```go
return server.HTTPResponse(c, &server.PostConfirmation{ // ← note the &
	Resource: "video", Message: "created", ID: "42",
})
```

answers `200` with `{"data":{"resource":"video","message":"created","id":"42"}}`
instead of the `201` with an unwrapped body that the value form produces. The
same applies to `*PatchConfirmation` and `*Confirmation`, which lose their
unwrapped shape.

This is a defect, not a design. It is currently **characterised by a test**
(`TestHTTPResponseCharacterizesPointerConfirmations` in `response_test.go`) so
that the behaviour is on the record and a fix has to be a deliberate, visible
change. Until it is fixed: **pass confirmations by value.**

## Logging

This package never constructs a logger and never writes anywhere else. The
caller passes one to `NewHTTP`, and every line the package emits — one entry per
request, plus the readiness lines — goes through it.

### The contract is three methods

```go
type Logger interface {
	Infow(msg string, keysAndValues ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
	Errorw(msg string, keysAndValues ...interface{})
}
```

That is all of it, and they are the three the package calls. The arguments after
`msg` are alternating keys and values — `"status", 503` — the convention zap's
`SugaredLogger` established and `slog` shares.

The interface is declared here, where it is consumed, rather than imported from
a logging library, so **using this package does not oblige you to adopt one**.
Nothing in the package proper imports a logger implementation.

### `log/slog`, with no dependency at all

```go
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Infow(msg string, kv ...interface{})  { s.l.Info(msg, kv...) }
func (s slogLogger) Warnw(msg string, kv ...interface{})  { s.l.Warn(msg, kv...) }
func (s slogLogger) Errorw(msg string, kv ...interface{}) { s.l.Error(msg, kv...) }

s, err := server.NewHTTP(nil, slogLogger{l: slog.New(slog.NewJSONHandler(os.Stdout, nil))})
```

Three one-line methods, and `log/slog` is standard library, so a service wired
this way adds **no logging dependency at all**. slog takes the same alternating
key/value arguments, so the adapter forwards them untouched; a logger with a
different shape — zerolog's fluent chain, logrus's `Fields` map — converts them
in these same three methods. [`examples/minimal`](examples/minimal) and
[`examples/streaming`](examples/streaming) run exactly this, and the adapter is
compiled and exercised by the package's own tests (`logger_test.go`), so it
cannot quietly stop working.

No adapter is exported from this package on purpose. It would be six lines of
code and a permanent piece of public API, and every consumer needs a slightly
different one — their own handler, their own level mapping, a request-scoped
logger pulled from a context — so the type belongs in the consumer.

### `logam` still works, unchanged

```go
log := logam.NewLogger(logam.Config{
	LogLevel:    "info",
	LogFormat:   "json",
	Environment: logam.EnvProduction,
})

s, err := server.NewHTTP(nil, log) // no adapter, no wrapper
```

`github.com/zionmedianetwork/logam`'s `Logger` carries seventeen methods; this
package calls three of them, and Go satisfies interfaces structurally, so a
`logam.Logger` **is** a `server.Logger`. Every call written before this
interface existed compiles unchanged, and a compile-time assertion in
`logam_compat_test.go` keeps it that way. That assertion, in a test file, is the
only reason logam is in this module's `go.mod` at all: nothing in the package
proper imports it, so it is never compiled into a consumer's binary and deleting
that one file is what would let `go mod tidy` drop it.
[`examples/readiness`](examples/readiness) and
[`examples/behind-proxy`](examples/behind-proxy) use logam.

### What gets logged

One structured entry per request. The rendering is your handler's; this came out
of `examples/minimal`'s slog JSON handler, wrapped here for width:

```json
{"time":"2026-08-09T16:48:54.907389-04:00","level":"INFO","msg":"request",
 "request_id":"mjQrrQrGccqoIIzbEtFEzvCXtJOJQDRd","method":"GET","path":"/v1/videos/42",
 "uri":"/v1/videos/42","status":200,"remote_ip":"127.0.0.1","latency":"135.11µs","bytes_out":42}
```

- `request_id` comes from the `RequestID` middleware and is the same value sent
  back in the `X-Request-ID` response header. A client-supplied `X-Request-ID`
  is preserved, so a correlation id survives the hop.
- `path` is the matched route pattern's path and `uri` the raw request URI,
  including the query string.
- `status` is what the client actually received: the global error handler runs
  before the values are read.
- An `error` field is added when the handler chain returned one.

Level: **error** when the handler returned an error **or** the status was 5xx;
**info** otherwise. Note the consequence — a handler that *returns*
`echo.NewHTTPError(400, ...)` is logged at error level, while one that writes
the same 400 with `c.JSON`/`c.NoContent` and returns `nil` is logged at info:

```json
{"level":"ERROR","msg":"request","method":"POST","path":"/v1/videos","status":400,
 "bytes_out":32,"error":"code=400, message=title is required",...}
```

(abridged: the other fields are as above.)

Liveness and readiness paths (`/healthz`, `/v1/healthz`, `/readyz`,
`/v1/readyz`) are skipped entirely, matched on the route pattern, so a query
string or trailing slash does not sneak them back into the log.

`Warnw` has exactly one caller: a failing readiness check. That line carries the
dependency's own error, which the probe response withholds unless `HTTP_DEBUG`
is set, and since the probe paths are outside the access log it is the only
place the cause is written down. Only transitions are logged, plus a repeat at
most once a minute while a check stays down, plus one line on recovery — see
[Health and readiness](#health-and-readiness).

Two things do not go through the injected logger: Echo's own startup line
(`⇨ http server started on :8080`, on stdout — the banner is hidden, the port
line is not), and Echo's internal logger for the rare case where writing an
error response itself fails.

## Examples

Four runnable programs, each in its own directory with its own README:

| Example | Demonstrates | Logger |
| --- | --- | --- |
| [`examples/minimal`](examples/minimal) | The smallest correct service, and the call site to copy. | `log/slog` |
| [`examples/readiness`](examples/readiness) | Readiness checks over stub dependencies you can knock over with curl. | `logam` |
| [`examples/behind-proxy`](examples/behind-proxy) | Production-shaped config behind a TLS-terminating load balancer. | `logam` |
| [`examples/streaming`](examples/streaming) | A request timeout that spares a streaming route. | `log/slog` |

Two of each on purpose: the stdlib pair shows that this package needs no
logging dependency, the logam pair that an existing `logam.Logger` is passed
straight in. Nothing else about the four differs in how they log.

They are part of this module, so `go build ./...`, `go vet ./...` and
`golangci-lint run ./...` cover them.

## Development

```bash
go build ./...
go vet ./...
gofmt -l .                       # must print nothing
go test -count=1 ./...
go test -race -count=1 ./...     # several tests are timing- and race-sensitive
go test -run '^TestName$' ./...  # a single test
golangci-lint run ./...          # config and rationale in .golangci.yml
```

CI pins the linter, so without a local install the exact version it runs is:

```bash
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./...
```

A `tool@version` run resolves outside this module, so it adds nothing to
`go.mod`.

`go.sum` **is** committed and `go.mod` is tidy, so a fresh clone builds
directly — there is no `go mod tidy` step to run first. CI
(`.github/workflows/ci.yml`) enforces both: it fails on any diff `go mod tidy`
would produce and on `go.sum` becoming untracked, then runs the commands above
plus `go mod verify` and `govulncheck`. A tidy diff in CI means something else
is wrong; do not "fix" it by regenerating the module files.

Adding a dependency — including for an example — changes `go.mod` and will fail
that check until the change is committed deliberately.

## Known limitations

Everything here is true of the code as it stands. None of it is a plan.

- **No TLS.** H2C only; termination must happen upstream. See
  [What it is not](#what-it-is-not).
- **No websocket support**, despite the historical README. Nothing here
  upgrades a connection.
- **The server type is unexported.** `NewHTTP` returns `*httpServer`, so
  consumers cannot name the type in a variable declaration, struct field or
  function signature. Use `:=`, or declare your own interface with the two or
  three methods you need.
- **The middleware stack and CORS policy are fixed at construction.** You can
  append middleware through `s.Echo()`, but not reorder or remove the built-in
  chain, and the CORS method list is not configurable.
- **CORS defaults to `*`.** Every origin is allowed unless
  `HTTP_ALLLOWED_ORIGINS` (three L's) is set. A deployment serving browser
  clients should set it.
- **Static files come from the process working directory.** The route is
  hardcoded as `e.Static("/static", "static")`, so it serves `./static`
  relative to wherever the binary was started. **`HTTP_STATIC_PATH` is read and
  defaulted but never used** — setting it has no effect at all.
- **`HTTPResponse` mishandles pointers**, as described in
  [Known issue](#known-issue-pointers-lose-their-status-code).
- **No security headers** (HSTS, `X-Content-Type-Options`, CSP, frame options),
  **no rate limiting**, **no gzip/compression**, and no metrics or tracing
  instrumentation. Add what you need with `s.Echo().Use(...)`, or put it in the
  proxy in front.

## License

MIT. See [LICENSE](LICENSE).
