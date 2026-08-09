# minimal

The smallest correct service built on this package, and the one to copy.

**It imports the standard library, Echo and this package. Nothing else.** In
particular it does not import `github.com/zionmedianetwork/logam`: the logger is
`log/slog`, adapted to `server.Logger` by three one-line methods, because
running this library requires no logging dependency at all.

## The logger, which is the only thing you have to supply

`NewHTTP` takes a `server.Logger`, and that interface is three methods:

```go
type Logger interface {
	Infow(msg string, keysAndValues ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
	Errorw(msg string, keysAndValues ...interface{})
}
```

The whole adapter in `main.go` is this — slog takes the same alternating
key/value arguments, so each method forwards them untouched:

```go
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Infow(msg string, kv ...interface{})  { s.l.Info(msg, kv...) }
func (s slogLogger) Warnw(msg string, kv ...interface{})  { s.l.Warn(msg, kv...) }
func (s slogLogger) Errorw(msg string, kv ...interface{}) { s.l.Error(msg, kv...) }

log := slogLogger{l: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
	Level: slog.LevelInfo,
}))}
```

A logger with a different shape — zerolog, logrus, a request-scoped logger of
your own — is converted in those same three methods. And if your service already
uses `logam`, there is nothing to convert: `logam.Logger` satisfies the interface
as it is, which is what [`../readiness`](../readiness) and
[`../behind-proxy`](../behind-proxy) show. Swap `NewJSONHandler` for
`NewTextHandler` here if `key=value` is easier to read while you curl at it.

Note that `serve` in `main.go` takes `server.Logger`, not the concrete adapter,
so `main` is the only place this program names a logger at all.

## What it demonstrates

- **A logger with no dependency behind it**, as above.
- **Configuration from the environment** with `server.NewHttpConfig()`, and a
  configuration error reported separately from a wiring error.
- **The three-call flow**: `NewHTTP(cfg, log)` → `s.Echo()` to register routes →
  `s.Run()`.
- **Both response shapes.** `GET /v1/videos/:id` returns an ordinary payload, so
  `HTTPResponse` answers 200 and wraps it as `{"data": ...}`.
  `POST /v1/videos` returns a `server.PostConfirmation`, so it answers 201 with
  the confirmation unwrapped. `&server.PostConfirmation{...}` produces the same
  response; it used not to, which is noted at the call site.
- **The call site for `Run()`'s error**, which is the thing this example exists
  for. `Run` never logs on your behalf and never exits the process, so a
  `main()` that ends in a bare `s.Run()` exits 0 and says nothing when the bind
  fails. Here the error is logged and the process exits 1.
- **Errors handed to callers deliberately**: `c.Bind`'s message names Go types
  and field offsets, so the handler answers with its own 400 message and
  attaches the real cause with `SetInternal`, where the access log picks it up.

## Run it

```bash
HTTP_BIND_ADDRESS=127.0.0.1:8080 go run ./examples/minimal
```

Every `HTTP_*` variable applies; none is required. Useful ones here:

| Variable | Effect |
| --- | --- |
| `HTTP_BIND_ADDRESS` | Listen address. Default `:8080`. |
| `HTTP_MAX_BODY_LIMIT` | Default `10M`. Set it to `10` (ten bytes) to watch the POST below get `413 Request Entity Too Large`. |
| `HTTP_DEBUG` | `true` indents JSON and copies handler errors into bodies. |
| `HTTP_SHUTDOWN_TIMEOUT` | Grace period on Ctrl-C. Default 10s. |

## Exercise it

Transcripts below are real, taken from `HTTP_BIND_ADDRESS=127.0.0.1:18081 go run
./examples/minimal` with the port rewritten to 8080 for readability.

```console
$ curl -s -i http://127.0.0.1:8080/healthz | head -1
HTTP/1.1 200 OK

$ curl -s -i http://127.0.0.1:8080/v1/videos/42 | sed -n '1p;$p'
HTTP/1.1 200 OK
{"data":{"id":"42","title":"Episode 42"}}

$ curl -s -i -X POST -H 'Content-Type: application/json' \
       -d '{"title":"Episode 1"}' http://127.0.0.1:8080/v1/videos | sed -n '1p;$p'
HTTP/1.1 201 Created
{"resource":"video","message":"created","id":"1"}

$ curl -s -i -X POST -H 'Content-Type: application/json' \
       -d '{}' http://127.0.0.1:8080/v1/videos | sed -n '1p;$p'
HTTP/1.1 400 Bad Request
{"message":"title is required"}

$ curl -s -i http://127.0.0.1:8080/readyz | sed -n '1p;$p'
HTTP/1.1 200 OK
{"status":"ok","checks":[]}
```

Readiness answers 200 with an empty list because no check has been registered:
nothing has declared a dependency, so there is nothing to be unready for. See
[`../readiness`](../readiness) for the other half.

## What the slog-backed logger writes

The curl session above, as it appeared on stdout. This is the point of the
example: the `request` entries are the package's (the first line is this
program's own startup log), the JSON is slog's, and no logging library was
involved in either.

```json
{"time":"2026-08-09T16:47:31.228451-04:00","level":"INFO","msg":"http server starting","bind_address":"127.0.0.1:8080","max_body_limit":"10M","real_ip_source":"peer","shutdown_timeout":"10s"}
{"time":"2026-08-09T16:48:54.907389-04:00","level":"INFO","msg":"request","request_id":"mjQrrQrGccqoIIzbEtFEzvCXtJOJQDRd","method":"GET","path":"/v1/videos/42","uri":"/v1/videos/42","status":200,"remote_ip":"127.0.0.1","latency":"135.11µs","bytes_out":42}
{"time":"2026-08-09T16:48:54.92628-04:00","level":"INFO","msg":"request","request_id":"nTrlYojiJiHOMcBKSsGonklHUmHCZQjJ","method":"POST","path":"/v1/videos","uri":"/v1/videos","status":201,"remote_ip":"127.0.0.1","latency":"127.817µs","bytes_out":50}
{"time":"2026-08-09T16:48:54.944821-04:00","level":"ERROR","msg":"request","request_id":"wtCRBHwrIFLdFMJRFCaVoILWdNjrruPW","method":"POST","path":"/v1/videos","uri":"/v1/videos","status":400,"remote_ip":"127.0.0.1","latency":"26.212µs","bytes_out":32,"error":"code=400, message=title is required"}
```

Four things to read out of that:

- **Neither probe is there.** `/healthz` and `/readyz` were both requested and
  both are excluded from the access log, matched on the route pattern.
- **The 400 is at `ERROR`, not `INFO`.** The handler *returned*
  `echo.NewHTTPError(400, ...)`, and the package logs at error level whenever
  the handler chain returned an error or the status was 5xx. Writing the same
  400 with `c.JSON` and returning `nil` would have been logged at info. That
  rule is the package's; slog only renders it.
- `request_id` is the value in the `X-Request-ID` response header. Send your own
  and it is preserved, so a correlation id survives the hop:

  ```console
  $ curl -s -H 'X-Request-ID: my-correlation-id' http://127.0.0.1:8080/v1/videos/7
  ```
  ```json
  {"level":"INFO","msg":"request","request_id":"my-correlation-id","method":"GET","path":"/v1/videos/7","status":200,...}
  ```

  (that one line abridged; the rest carry the same fields as above.)

- One line on stdout is **not** slog's, and cannot be: Echo's own
  `⇨ http server started on 127.0.0.1:8080`. The banner is hidden; that line is
  not, and it does not go through the injected logger.

Press Ctrl-C (or send SIGTERM) and the server drains and exits 0.

## Pointers and values are the same response

Change the `POST` handler to pass a pointer:

```go
return server.HTTPResponse(c, &server.PostConfirmation{...})
```

and the request answers `201` with
`{"resource":"video","message":"created","id":"1"}`, byte for byte what the
value form produces. That is worth trying precisely because it used to differ:
the type switch in `response.go` matched value types only, and the pointer form
came back `200` with the body wrapped in `data`. It now names both forms of all
three confirmation types.

A *nil* pointer is the one case that is not a confirmation:
`var c *server.PostConfirmation` answers `200 {"data":null}` rather than
claiming a creation with an empty body.
