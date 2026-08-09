# server

`github.com/zionmedianetwork/server` is a small, opinionated wrapper around
[Echo v4](https://echo.labstack.com/) that gives a Go service an HTTP listener
with the parts every service needs already wired: structured access logging
through a logger you inject, a request id, a body limit, panic recovery, CORS
that is closed until you open it, baseline security headers, optional rate
limiting and compression, liveness and readiness endpoints, an optional request
timeout, and a graceful shutdown that drains in-flight requests on
`SIGINT`/`SIGTERM`.

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
| An entry in `HTTP_ALLLOWED_ORIGINS` that is not a bare origin | `invalid allowed origin "https://app.example.com/"` |
| `*` in `HTTP_ALLLOWED_ORIGINS` alongside named origins | `allows every origin, so the named ones are never consulted` |
| `HTTP_CORS_MAX_AGE` or `HTTP_HSTS_MAX_AGE` negative, or under `1s` | `truncates to zero and disables it` |
| `HTTP_HSTS_MAX_AGE` set while security headers are disabled | `drop HTTP_DISABLE_SECURITY_HEADERS or drop HTTP_HSTS_MAX_AGE` |
| `HTTP_HSTS_INCLUDE_SUBDOMAINS` with no `HTTP_HSTS_MAX_AGE` | `no Strict-Transport-Security header is sent` |
| `HTTP_RATE_LIMIT` negative, or `HTTP_RATE_LIMIT_BURST` set without it | `set HTTP_RATE_LIMIT or drop HTTP_RATE_LIMIT_BURST` |

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
construction. The first entry is the outermost wrapper, so everything below it
sees the responses — including the rejections — that the ones after it produce:

1. `Pre`: `RemoveTrailingSlash`, `RequestID`
2. `Use`: security headers — unless `HTTP_DISABLE_SECURITY_HEADERS` is set
3. `Use`: CORS
4. `Use`: `BodyLimit`, request logger, `Recover`
5. `Use`: rate limiter — only when `HTTP_RATE_LIMIT` is set
6. `Use`: gzip — only when `HTTP_GZIP` is set
7. `Use`: request timeout — only when `HTTP_REQUEST_TIMEOUT` is set

Three placements are load-bearing. **CORS sits outside everything that can
refuse a request**, so a `413` from the body limit, a `429` from the rate
limiter and the `500` from a recovered panic all carry
`Access-Control-Allow-Origin` — a cross-origin response without it is one the
browser will not show the script that asked, status and all, so a rejected
upload would otherwise look like a CORS misconfiguration rather than a file that
was too big. **The rate limiter sits inside the access log**, so every `429` is
logged; a limit you cannot see biting is indistinguishable from a client that
stopped calling. **The access log sits outside `Recover`**, so a panicking
handler is logged: Echo's request logger builds its entry *after* `next(c)`
returns and does not defer that, so a panic unwinding through it skips the log
entirely — with `Recover` outside, as it was until now, a 500 from a panic
produced no access log line at all. Inside, the panic has become a response
before the logger sees it.

The cost of the first one: a CORS preflight is answered above the access log, so
`OPTIONS` requests are not logged. They are cached by the browser for
`HTTP_CORS_MAX_AGE` anyway.

The cost of the third one is small but worth knowing: `Recover` no longer wraps
the logger, so a `Logger` implementation that panics is not contained by this
package. That is a bug in the logger, it can only happen after the response has
been written, and `net/http` still stops it taking the process down.

You may append your own with `e.Use(...)` or `e.Pre(...)`, but you cannot
reorder or remove the built-ins, and anything you append with `Use` runs
*inside* all of them.

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
| `HTTP_ALLLOWED_ORIGINS` | `AlllowedOrigins` | **empty** | CORS origin allowlist. Empty allows **no** cross-origin access. Note the spelling. See [CORS](#cors). |
| `HTTP_ALLOWED_HEADERS` | `AllowedHeaders` | `Accept,Authorization,Content-Type,X-Request-Id` | Request headers a preflight allows. **One L**, unlike the line above. |
| `HTTP_CORS_MAX_AGE` | `CorsMaxAge` | `1h` | How long a browser may cache a preflight. `0` sends no `Access-Control-Max-Age` at all. |
| `HTTP_DISABLE_SECURITY_HEADERS` | `DisableSecurityHeaders` | `false` | Turns off the baseline response headers. See [Security headers](#security-headers). |
| `HTTP_HSTS_MAX_AGE` | `HstsMaxAge` | `0` (off) | `Strict-Transport-Security` lifetime. Only sent for requests that arrived over TLS or carried `X-Forwarded-Proto: https`. |
| `HTTP_HSTS_INCLUDE_SUBDOMAINS` | `HstsIncludeSubdomains` | `false` | Adds `includeSubdomains`. Inert — and refused — without a max age. |
| `HTTP_RATE_LIMIT` | `RateLimit` | `0` (off) | Requests per second per client address. See [Rate limiting](#rate-limiting). |
| `HTTP_RATE_LIMIT_BURST` | `RateLimitBurst` | one second's worth | Requests allowed to arrive at once. Refused while the rate limit is off. |
| `HTTP_GZIP` | `Gzip` | `false` | Compresses responses over 1 KiB, skipping probe paths and `HTTP_TIMEOUT_EXEMPT_PATHS`. |
| `HTTP_STATIC_PATH` | `StaticPath` | `/static` | **Currently ignored.** See [Known limitations](#known-limitations). |
| `HTTP_DEBUG` | `Debug` | `false` | Copies handler errors into response bodies, indents all JSON, and adds the cause to readiness reports. |
| `HTTP_REAL_IP_SOURCE` | `RealIPSource` | `peer` | `peer` or `xff`. See [Client IP](#client-ip). |
| `HTTP_TRUSTED_PROXIES` | `TrustedProxies` | empty | Comma-separated CIDRs allowed to relay a client address. Only valid with `xff`. |

Not configurable: the HTTP/2 settings (`MaxConcurrentStreams: 200`,
`MaxReadFrameSize: 1024000`, `IdleTimeout: 10s`), the CORS method list
(`GET, POST, PUT, PATCH, DELETE`), `Access-Control-Allow-Credentials` (never
sent — see [CORS](#cors)), the security header *values*, and the middleware
stack.

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
   `HTTP_ALLOWED_ORIGINS` still does nothing at all. What has changed is the
   consequence: the origin list now defaults to empty, so the typo means *no
   cross-origin access* rather than *every origin*, and it announces itself the
   first time a browser calls. Note also its new neighbour
   `HTTP_ALLOWED_HEADERS`, which has **one** L and is correctly spelled — the
   two sit next to each other in a config file and are easy to skim past. The
   misspelling is also visible in Go code for anyone building an `HttpConfig` by
   hand.

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

`HTTP_TIMEOUT_EXEMPT_PATHS` has a second effect: those routes are also skipped
by gzip when `HTTP_GZIP` is on. See [Compression](#compression).

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

## CORS

**The default allows nothing.** With `HTTP_ALLLOWED_ORIGINS` unset, no origin
gets `Access-Control-Allow-Origin`, so no browser hands a response from this
server to a script served from anywhere else.

That is a change. The default used to be `*`, which meant every service that
never set the variable published every route — `GET`, `POST`, `PUT`, `PATCH`
and `DELETE` alike — to any page that could get a browser to call it, and
nothing in that deployment's configuration recorded the decision. **To restore
the old behaviour exactly, set `HTTP_ALLLOWED_ORIGINS=*`.**

```bash
HTTP_ALLLOWED_ORIGINS=https://app.example.com,https://admin.example.com
```

Entries are origins: a scheme and a host, optionally a port, and nothing else.
`https://app.example.com/` (trailing slash), `app.example.com` (no scheme) and
`https://app.example.com/videos` (a path) are all refused by `NewHTTP`, because
none of them can ever equal the `Origin` header a browser sends and each would
sit in the config looking like an allowance that works. Echo's wildcard host
patterns are supported: `https://*.example.com`.

Listing `*` alongside named origins is refused too. The wildcard matches first
and matches everything, so such a list reads as a narrow policy and behaves as
an open one.

Three more things:

- **Credentials are never allowed.** This package does not send
  `Access-Control-Allow-Credentials` and has no setting for it, so even
  `HTTP_ALLLOWED_ORIGINS=*` cannot be used to read a response that depends on
  the caller's cookies. Enabling credentials belongs with whoever owns the
  authentication scheme, and credentials-with-a-wildcard is the CORS
  misconfiguration everybody writes about.
- **Preflights are cached for an hour.** `Access-Control-Max-Age` was never
  sent, so browsers re-issued an `OPTIONS` preflight before *every* non-simple
  cross-origin request. The default is now `1h`: browsers cap this themselves
  (Chrome at 2 hours, Firefox at 24), so a larger number is silently truncated
  and buys nothing, while an hour is honoured verbatim everywhere and bounds
  how long a tightened policy keeps working in an already-open tab.
  `HTTP_CORS_MAX_AGE=0` restores the old "no header" behaviour.
- **Request headers are an allowlist.** `AllowHeaders` was unset, which made
  echo reflect whatever the browser asked for. The default is now
  `Accept,Authorization,Content-Type,X-Request-Id` — the last one so a browser
  client can send the correlation id this package logs and echoes back.
  `HTTP_ALLOWED_HEADERS=*` restores "anything the browser asks for"; note that
  the fetch standard excludes `Authorization` from that wildcard, so list it by
  name if you need it.

CORS is enforced by the browser, not by this server: a refused origin still gets
a normal `200` (or a `204` for a preflight) with the header simply absent.
Testing it with curl will mislead you.

## Security headers

On by default, on every response including the rejections:

| Header | Value | Why |
| --- | --- | --- |
| `X-Content-Type-Options` | `nosniff` | Stops a browser second-guessing a `Content-Type`, which is how an upload or JSON endpoint ends up executing as script. |
| `X-Frame-Options` | `SAMEORIGIN` | Refuses cross-origin framing. Not `DENY`: this package serves a same-origin static route, and `DENY` would break an embed the deployment owns while adding nothing. |
| `Referrer-Policy` | `strict-origin-when-cross-origin` | The paths here name resources (`/v1/videos/42`); this keeps them out of a third party's `Referer`. |
| `X-XSS-Protection` | `0` | Deliberate, and not echo's default of `1; mode=block`. The legacy auditor that enables was itself exploitable and is gone from every current browser; `0` is what OWASP now recommends, sent rather than omitted so a browser that still has the auditor does not switch it on itself. |

`HTTP_DISABLE_SECURITY_HEADERS=true` turns all four off and restores the
previous behaviour exactly.

No `Content-Security-Policy` is set. A useful one names a particular
application's script, style and frame origins, and a policy that guesses wrong
does not fail loudly — it silently stops half a page loading. Add your own with
`s.Echo().Use(...)`.

### HSTS is off by default, and that is deliberate

`Strict-Transport-Security` is not sent unless `HTTP_HSTS_MAX_AGE` is set.

Every other header here describes one response. HSTS is a durable instruction:
for `max-age` seconds afterwards the browser refuses to speak plaintext to that
host, and the only way to undo it early is to serve `max-age=0` over working
HTTPS. This process speaks **h2c with no TLS of its own**, so whether the origin
it is reached at is HTTPS everywhere is a fact about somebody's ingress that
this package cannot see. A library that emitted HSTS by default could take a
hostname offline, on upgrade, for whatever plaintext still depends on it.

When you do set it:

```bash
HTTP_HSTS_MAX_AGE=2160h              # ninety days; Go durations have no "d"
HTTP_HSTS_INCLUDE_SUBDOMAINS=true
```

That is sent as `Strict-Transport-Security: max-age=7776000; includeSubdomains`.

- The header is emitted only for requests that arrived over TLS or carried
  `X-Forwarded-Proto: https`. Behind a terminating proxy that means **the proxy
  must set that header, and must be the only thing that can** — the same
  requirement `HTTP_REAL_IP_SOURCE` documents for `X-Forwarded-For`.
- `includeSubdomains` is off by default because it is the irreversible part: it
  pins every name under the domain, including ones this service knows nothing
  about. It is refused without a max age, since that combination sends no header
  at all while reading like HSTS is configured.
- There is no `preload` and there will not be one. That is a submission to a
  list compiled into browsers, it takes months to leave, and no library should
  be able to put a consumer's domain on it.

## Rate limiting

Off by default. `HTTP_RATE_LIMIT` is requests per second per client address:

```bash
HTTP_RATE_LIMIT=20
HTTP_RATE_LIMIT_BURST=60     # optional; defaults to one second's worth
```

Over the limit the caller gets `429 Too Many Requests`, and the refusal is in
the access log like any other response.

Off by default because three of its properties are facts about a deployment that
this package cannot see:

- **The limiter is per process.** Ten replicas of a service configured at 20
  req/s enforce 200 req/s in aggregate, and that number changes every time the
  deployment scales.
- **The store is in memory.** It holds one bucket per distinct client address,
  dropped after three idle minutes. Behind a proxy that is a small map; facing
  the open internet directly it is a function of your attacker's address space.
- **It is keyed on `c.RealIP()`**, so it inherits `HTTP_REAL_IP_SOURCE`. With
  the wrong source every caller shares one bucket, or every caller can mint a
  new one at will — and a limit keyed on a spoofable address is worse than no
  limit, because it looks like protection. Read [Client IP](#client-ip) before
  switching this on.

`/healthz`, `/v1/healthz`, `/readyz` and `/v1/readyz` are never rate limited. A
`429` on readiness withdraws the instance from load balancing and a `429` on
liveness invites an orchestrator to restart it, which would turn a burst of
client traffic into an outage by way of the probes.

A burst is derived from the rate when you do not set one — rounded **up**, and
never zero, because the underlying store treats a zero burst literally and
refuses everything: `HTTP_RATE_LIMIT=0.5` with no burst would otherwise
configure an outage. Setting `HTTP_RATE_LIMIT_BURST` while the rate limit is off
is a startup error rather than a no-op.

## Compression

Off by default. `HTTP_GZIP=true` compresses responses over 1 KiB for clients
that send `Accept-Encoding: gzip`.

Off by default because this package carries media. Compressing an MP4 or a JPEG
spends CPU on both ends to produce something no smaller, and compression
buffers: a streaming or SSE route only keeps streaming because echo's writer
forces a gzip flush on each `Flush()`, and the proxies in front are under no
such obligation.

When it is on, two sets of routes are skipped:

- the probe paths, whose bodies are two words and would come out *larger*;
- **every route named in `HTTP_TIMEOUT_EXEMPT_PATHS`.** That list already names
  the routes a deployment has declared long-running — the streams, the
  downloads, the uploads — which is exactly the set compression should stay out
  of. One list, so the two cannot drift apart. See
  [`examples/streaming`](examples/streaming).

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

A panicking handler is logged too, and its entry has a shape of its own worth
recognising: **status 500, error level, and no `error` field.** `Recover` runs
inside the logger and handles the recovered panic itself, so by the time the
entry is built there is no error left to report — the level comes from the
status, not from the error. The panic value and its stack go to stderr through
Echo's own recoverer and never reach the injected logger, so a 500 with no
`error` field is the line that tells you to go and find them.

```json
{"level":"ERROR","msg":"request","method":"GET","path":"/v1/videos/42","status":500,
 "bytes_out":36,"request_id":"...","latency":"212.4µs",...}
```

Liveness and readiness paths (`/healthz`, `/v1/healthz`, `/readyz`,
`/v1/readyz`) are skipped entirely, matched on the route pattern, so a query
string or trailing slash does not sneak them back into the log.

CORS preflights are absent too, for a different reason: the CORS middleware
answers an `OPTIONS` above the logger, which is the placement that lets every
rejection below it carry CORS headers — see the middleware stack under
[The consumer flow](#the-consumer-flow).

`Warnw` has exactly one caller: a failing readiness check. That line carries the
dependency's own error, which the probe response withholds unless `HTTP_DEBUG`
is set, and since the probe paths are outside the access log it is the only
place the cause is written down. Only transitions are logged, plus a repeat at
most once a minute while a check stays down, plus one line on recovery — see
[Health and readiness](#health-and-readiness).

Three things do not go through the injected logger: Echo's own startup line
(`⇨ http server started on :8080`, on stdout — the banner is hidden, the port
line is not), Echo's internal logger for the rare case where writing an error
response itself fails, and the `[PANIC RECOVER]` line with the stack trace,
which `Recover` prints to stderr. The access log records *that* a request
panicked; the stack is only on stderr.

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
- **CORS is closed until you open it.** No origin is allowed unless
  `HTTP_ALLLOWED_ORIGINS` (three L's) names it. A deployment serving browser
  clients must set it; `*` is still available and still a decision.
- **CORS preflights are not in the access log.** The middleware answers them
  above the logger, which is what lets every rejection below carry CORS headers.
  A browser's own network panel is where a failing preflight is diagnosed.
- **A `413` from a declared `Content-Length` is not in the access log either.**
  `BodyLimit` sits above the logger and refuses that request before calling the
  rest of the chain, so nothing below it runs. An oversize body with no declared
  length *is* logged, because the limit is then hit during the handler's read
  and surfaces as a returned error. The response is correct in both cases —
  `413`, with CORS and security headers — only the log line differs.
- **The rate limiter is per process and in memory.** See
  [Rate limiting](#rate-limiting) — the effective fleet-wide limit is your
  replica count times the configured one.
- **Static files come from the process working directory.** The route is
  hardcoded as `e.Static("/static", "static")`, so it serves `./static`
  relative to wherever the binary was started. **`HTTP_STATIC_PATH` is read and
  defaulted but never used** — setting it has no effect at all.
- **`HTTPResponse` mishandles pointers**, as described in
  [Known issue](#known-issue-pointers-lose-their-status-code).
- **No `Content-Security-Policy` and no HSTS by default**, and no metrics or
  tracing instrumentation. See [Security headers](#security-headers) for why
  those two are left to the consumer; add the rest with `s.Echo().Use(...)`, or
  put it in the proxy in front.

## License

MIT. See [LICENSE](LICENSE).
