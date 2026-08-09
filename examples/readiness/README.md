# readiness

Three stub dependencies registered as readiness checks, and a debug route that
knocks them over so you can watch `/readyz` flip while `/healthz` never moves.

## A note on the logger

This example logs through `github.com/zionmedianetwork/logam`, and passes the
result of `logam.NewLogger(...)` to `NewHTTP` with **no adapter and no wrapper**.

That is worth saying out loud, because logam is **one option among several, not
a requirement**. `NewHTTP` takes a `server.Logger`, which is three methods —
`Infow`, `Warnw`, `Errorw` — and `logam.Logger` happens to have all three among
its seventeen. Go satisfies interfaces structurally, so it is accepted as it is,
and every call site written before that interface existed compiles unchanged.

A service that does not use logam supplies its own logger instead: the three-line
`log/slog` adapter in [`../minimal`](../minimal) and
[`../streaming`](../streaming) is the whole of what that takes, and needs nothing
outside the standard library. This example keeps logam because its subject *is*
the log — the warn line below is the only place a failing check's cause is ever
written down — and because logam's console format is what the transcripts here
show.

## Liveness versus readiness

Two questions that look similar and must never be answered by the same
endpoint.

| | Liveness | Readiness |
| --- | --- | --- |
| Paths | `/healthz`, `/v1/healthz` | `/readyz`, `/v1/readyz` |
| Question | "Is this process running and answering?" | "Should this instance be sent traffic?" |
| Depends on | Nothing. Static 200. | Every registered check. |
| Failure means | The process is wedged. | A dependency is unusable right now. |
| Correct reaction | Restart the pod. | Take it out of the load balancer and wait. |

If liveness consulted the database, a database outage would fail liveness on
every pod at once and the orchestrator would restart all of them — turning a
dependency outage into a fleet-wide crash loop, while the database is still
down and the restarts fix nothing. That is why the liveness handler in this
package is a static `200 OK` and cannot be made to do anything else.

In Kubernetes:

```yaml
livenessProbe:
  httpGet: { path: /healthz, port: 8080 }
readinessProbe:
  httpGet: { path: /readyz, port: 8080 }
startupProbe:
  httpGet: { path: /readyz, port: 8080 }
```

## What it demonstrates

- `RegisterReadinessCheck` with its error handled — registration is refused for
  an empty name, a nil check or a duplicate name.
- Checks that **respect the context** they are handed, which is what the
  package requires: the probe abandons a check that overruns
  `HTTP_READINESS_TIMEOUT`, but the goroutine behind it keeps running.
- Checks running **concurrently**: the slowest stub takes 40ms and the probe
  costs about that, not the sum.
- Report ordering — the checks appear in registration order, which is why the
  example keeps them in a slice rather than a map.
- The **cause withheld from the body but always logged at warn**, and the
  transition-only logging (fail once, recover once) that keeps a fleet of pods
  from flooding the log during an outage.
- Application traffic continuing to be served while the instance is unready:
  readiness informs the load balancer, it does not make the process refuse
  requests it still receives.

The `POST /debug/dependencies/:name/:state` route exists only so this can be
driven with curl. **Do not ship anything like it**: an endpoint that makes an
instance unready from the outside is a denial of service with a friendly URL.

## Run it

```bash
HTTP_BIND_ADDRESS=127.0.0.1:8080 go run ./examples/readiness
```

| Variable | Effect |
| --- | --- |
| `HTTP_READINESS_TIMEOUT` | Bound on one probe, covering all checks. Default `2s`. |
| `HTTP_DEBUG` | `true` adds the failing dependency's error to the readiness body. Off by default, and off is right for anything exposed. |

## Exercise it

Everything healthy:

```console
$ curl -s -i http://127.0.0.1:8080/readyz | sed -n '1p;$p'
HTTP/1.1 200 OK
{"status":"ok","checks":[{"name":"postgres","status":"ok"},{"name":"redis","status":"ok"},{"name":"object-store","status":"ok"}]}
```

Knock the database over:

```console
$ curl -s -X POST http://127.0.0.1:8080/debug/dependencies/postgres/down
{"message":"dependency \"postgres\" is now down"}

$ curl -s -i http://127.0.0.1:8080/readyz | sed -n '1p;$p'
HTTP/1.1 503 Service Unavailable
{"status":"fail","checks":[{"name":"postgres","status":"fail"},{"name":"redis","status":"ok"},{"name":"object-store","status":"ok"}]}
```

Liveness is unmoved, and so is the application route:

```console
$ curl -s -i http://127.0.0.1:8080/healthz | head -1
HTTP/1.1 200 OK

$ curl -s -i http://127.0.0.1:8080/v1/videos | sed -n '1p;$p'
HTTP/1.1 200 OK
{"data":["episode-1","episode-2"]}
```

The body named the failing dependency but not why. The log did:

```
WARN  readiness check failed  {"check": "postgres",
      "error": "pq: dial tcp 10.0.0.7:5432: connect: connection refused (user \"zion_admin\")",
      "failing_for": "0s"}
```

That is the point of the split: `pq: ... user "zion_admin"` names a host, a port
and an account, and readiness is routinely exposed further than the rest of the
API. Poll `/readyz` in a loop and you will see the 503 repeat while the log
stays quiet — only transitions are logged, plus a repeat at most once a minute.

Set `HTTP_DEBUG=true` and the same body carries the cause:

```json
{"status":"fail","checks":[{"name":"postgres","status":"fail","error":"pq: dial tcp 10.0.0.7:5432: connect: connection refused (user \"zion_admin\")"}]}
```

Bring it back:

```console
$ curl -s -X POST http://127.0.0.1:8080/debug/dependencies/postgres/up
{"message":"dependency \"postgres\" is now up"}
```

```
INFO  readiness check recovered  {"check": "postgres", "failing_for": "204.22331ms"}
```

Both readiness paths behave identically — `/readyz` and `/v1/readyz` — and both,
like the liveness paths, are excluded from the access log.
