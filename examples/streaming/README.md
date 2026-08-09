# streaming

A request timeout tight enough to protect an ordinary API, and a streaming
route that must not be killed by it. This is the media-service case, and it is
the configuration most likely to be got wrong, because getting it wrong looks
like the feature is broken rather than misconfigured.

## What it demonstrates

Three routes, deliberately different:

| Route | Exempt? | Behaviour |
| --- | --- | --- |
| `GET /v1/videos/:id/stream` | yes | Writes ten chunks a second apart and completes, ten seconds later. |
| `GET /v1/videos/:id` | no | Propagates the request context into a five-second dependency call; answered `503` at the deadline. |
| `GET /v1/videos/:id/transcode` | no | Ignores its context. Sleeps four seconds and answers `200` anyway. |

The third one is the point of the example as much as the first two. The timeout
is a deadline on `c.Request().Context()`, and cancellation is a message, not a
stop: a handler that does not listen is not bounded by anything.

Like [`../minimal`](../minimal), this program logs through **`log/slog` from the
standard library** — the same three-line adapter to `server.Logger` at the top of
`main.go`, and no `logam` import. It is the second stdlib example because it is
the one that exercises both levels the access log uses, info for the stream that
completes and error for the route the timeout cuts off, so the transcripts below
show that the level rule belongs to this package rather than to any particular
logging library. [`../readiness`](../readiness) and
[`../behind-proxy`](../behind-proxy) use `logam` instead, which satisfies
`server.Logger` with no adapter at all.

## Run it

```bash
HTTP_BIND_ADDRESS=127.0.0.1:8080 \
HTTP_REQUEST_TIMEOUT=2s \
HTTP_TIMEOUT_EXEMPT_PATHS=/v1/videos/:id/stream \
HTTP_WRITETIMEOUT=120s \
go run ./examples/streaming
```

| Variable | Why it is set |
| --- | --- |
| `HTTP_REQUEST_TIMEOUT=2s` | The bound. Off by default; this package will not guess one for routes it cannot see. |
| `HTTP_TIMEOUT_EXEMPT_PATHS` | The **route pattern**, `/v1/videos/:id/stream` — not a request URL, and not `/v1/videos/42/stream`. |
| `HTTP_WRITETIMEOUT=120s` | The one everybody forgets. See below. Note the spelling: no underscore in the middle. |

## The `WriteTimeout` interaction

Exempting a route from the request timeout is **not enough**.
`http.Server.WriteTimeout` closes the connection when it passes, whatever any
middleware thinks, so the transport cuts a long stream off regardless of its
exemption. With `HTTP_WRITETIMEOUT` left at its 15s default, a ten second stream
survives by luck and a twenty second one does not. Lower it to prove the point:

```console
$ HTTP_WRITETIMEOUT=3s HTTP_REQUEST_TIMEOUT=2s \
  HTTP_TIMEOUT_EXEMPT_PATHS=/v1/videos/:id/stream go run ./examples/streaming
$ curl -s -N http://127.0.0.1:8080/v1/videos/42/stream
chunk 1 of 10
chunk 2 of 10
curl: (18) transfer closed with outstanding read data remaining
```

The exemption did exactly what it promised — no deadline was put on the request
context — and the connection was severed anyway, three seconds in, with no
status and no explanation. Raise `HTTP_WRITETIMEOUT` above the longest response
any exempt route can produce.

The same variable also constrains the request timeout from the other side.
`NewHTTP` refuses a request timeout at or above the write timeout, because the
connection would be closed before the 503 could be written:

```console
$ HTTP_REQUEST_TIMEOUT=30s go run ./examples/streaming
{"time":"2026-08-09T16:51:09.155266-04:00","level":"ERROR","msg":"http server stopped","error":"build http server: request timeout 30s must be below the write timeout 15s, or the connection is closed before a 503 can be written and the timeout never produces a response: lower HTTP_REQUEST_TIMEOUT or raise HTTP_WRITETIMEOUT"}
exit status 1
```

## Exercise it

**The exempt stream runs to completion**, well past the 2s request timeout:

```console
$ time curl -s -N http://127.0.0.1:8080/v1/videos/42/stream
chunk 1 of 10
...
chunk 10 of 10
curl -s -N http://127.0.0.1:8080/v1/videos/42/stream  0.01s user 0.01s system 0% cpu 10.019 total
```

```json
{"time":"2026-08-09T16:50:51.412125-04:00","level":"INFO","msg":"request","request_id":"hhkZEnCfqGCRvfLGlotPOGEAUwBbqopd","method":"GET","path":"/v1/videos/42/stream","uri":"/v1/videos/42/stream","status":200,"remote_ip":"127.0.0.1","latency":"10.000258937s","bytes_out":141}
```

**The ordinary route is cut off at the deadline**, with a status the client can
act on:

```console
$ time curl -s -i http://127.0.0.1:8080/v1/videos/42 | sed -n '1p;$p'
HTTP/1.1 503 Service Unavailable
{"message":"Service Unavailable"}
curl -s -i http://127.0.0.1:8080/v1/videos/42  0.01s user 0.01s system 0% cpu 2.020 total
```

Two seconds, not five. The handler passed `c.Request().Context()` into the call
that blocks and returned the resulting error wrapped with `%w`, so `errors.Is`
still finds `context.DeadlineExceeded` and the middleware turns it into the
503. The access log keeps the real cause:

```json
{"time":"2026-08-09T16:50:53.432085-04:00","level":"ERROR","msg":"request","request_id":"ZeGDTbiPqPFfCZLYYeFxQPGpGZEghjnM","method":"GET","path":"/v1/videos/42","uri":"/v1/videos/42","status":503,"remote_ip":"127.0.0.1","latency":"2.000443649s","bytes_out":34,"error":"code=503, message=Service Unavailable, internal=look up video metadata: context deadline exceeded"}
```

`ERROR` because the status was 5xx; the stream above it was `INFO`. Both lines
came out of the same three-method adapter over `slog`.

**The uncooperative route ignores all of it:**

```console
$ time curl -s -i http://127.0.0.1:8080/v1/videos/42/transcode | sed -n '1p;$p'
HTTP/1.1 200 OK
{"message":"transcode of video 42 finished, late and unbothered"}
curl -s -i http://127.0.0.1:8080/v1/videos/42/transcode  0.01s user 0.01s system 0% cpu 4.023 total
```

Four seconds and a 200, under a two second timeout. Nothing was cancelled,
because nothing was listening. The handler held its goroutine and its
connection for the full duration, exactly as it would with no timeout
configured — and if it had run past `HTTP_WRITETIMEOUT`, the client would have
been dropped with no status at all.

The lesson is worth stating plainly: `HTTP_REQUEST_TIMEOUT` bounds handlers
that propagate `c.Request().Context()` into every blocking call and hand the
resulting error back. For anything else it is decoration. A handler that
notices the cancellation but returns some unrelated error is a third case: it
gets whatever that error maps to, usually a 500, instead of the 503.

## Streaming handler notes

- The status is committed with `res.WriteHeader(http.StatusOK)` before the body
  starts, so a failure partway through has no status left to send. Echo's error
  handler checks for that and stays out of the way; returning the error still
  records it in the access log, which is the only place it can be reported.
- `res.Flush()` after each chunk is what makes it a stream. Without it the
  chunks sit in the buffer and arrive together at the end.
- The exempt handler still watches its context. It carries no deadline, but the
  client hanging up cancels it, and there is no reason to keep producing bytes
  nobody will read.
