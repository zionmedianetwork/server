# behind-proxy

Production-shaped configuration for running behind a TLS-terminating load
balancer, and a `/v1/whoami` route that makes the one setting people get wrong
visible in a single response.

This server speaks **HTTP/2 cleartext**. There is no TLS here and no way to add
it, so something in front must terminate it. Everything below assumes that
something exists; without it, none of this configuration matters because the
traffic is already in the clear.

## A note on the logger

Like [`../readiness`](../readiness), this example logs through
`github.com/zionmedianetwork/logam` and hands `logam.NewLogger(...)` straight to
`NewHTTP` — no adapter, no wrapper. `NewHTTP` asks for a `server.Logger`, which
is `Infow`, `Warnw` and `Errorw` and nothing else; `logam.Logger` has those three
among its seventeen and satisfies the interface structurally.

logam is **one option among several**, not a requirement of this package. It is
used here because this is the deployment-shaped example and it is what these
services run in production. A consumer without it writes the three-line `log/slog`
adapter shown in [`../minimal`](../minimal) and depends on nothing but the
standard library — the startup warnings below, which are ordinary `Warnw` calls,
come out the same either way.

## What it demonstrates

- `HTTP_REAL_IP_SOURCE=xff` with `HTTP_TRUSTED_PROXIES`, and what changes when
  you set them.
- A named `HTTP_ALLLOWED_ORIGINS` (three L's — the struct field is misspelled
  and the variable inherits it). Unset, **no cross-origin access is allowed at
  all**; the browser clients a deployment serves have to be named.
- `HTTP_DEBUG` left off, so handler errors are not narrated to callers.
- A body limit sized for an API rather than for uploads.
- Reading the resolved `*server.HttpConfig` back at startup and **warning about
  the settings that are still at development defaults**. That is worth doing in
  a real service: these particular defaults fail open and are otherwise
  invisible until an audit needs them.

## Run it

Default mode — `HTTP_REAL_IP_SOURCE` unset, so forwarding headers are ignored:

```bash
HTTP_BIND_ADDRESS=127.0.0.1:8080 go run ./examples/behind-proxy
```

```
WARN  client address is the peer address and forwarding headers are ignored  {"variable": "HTTP_REAL_IP_SOURCE", ...}
```

(There is no CORS warning here any more: with `HTTP_ALLLOWED_ORIGINS` unset the
server allows no cross-origin access, so there is nothing to warn about. Set it
to `*` and the `cors allows every origin` line comes back.)

Deployment mode — the load balancer relays the client address and is trusted to:

```bash
HTTP_BIND_ADDRESS=127.0.0.1:8080 \
HTTP_REAL_IP_SOURCE=xff \
HTTP_TRUSTED_PROXIES=127.0.0.0/8,10.4.0.0/16 \
HTTP_ALLLOWED_ORIGINS=https://zion.example,https://admin.zion.example \
HTTP_MAX_BODY_LIMIT=2M \
HTTP_REQUEST_TIMEOUT=10s \
go run ./examples/behind-proxy
```

(`127.0.0.0/8` is in the list only so that curl from your own machine plays the
part of the proxy. A real deployment lists the load balancer's ranges and
nothing else.)

## The two curl invocations

The same spoofed header, against the two configurations.

**Default (`peer`) — the header is ignored:**

```console
$ curl -s -H 'X-Forwarded-For: 203.0.113.9' -H 'X-Real-IP: 203.0.113.9' \
       http://127.0.0.1:8080/v1/whoami
{"data":{"client_ip":"127.0.0.1","peer":"127.0.0.1:59984","x_forwarded_for":"203.0.113.9","x_real_ip":"203.0.113.9","proto":"HTTP/1.1"}}
```

`client_ip` is the address that actually opened the connection. The caller
asked to be `203.0.113.9` and was not believed. Anything built on `c.RealIP()`
— the access log, an allowlist, a rate limiter — sees the truth.

**`xff` with the caller's range trusted — the header is honoured:**

```console
$ curl -s -H 'X-Forwarded-For: 203.0.113.9' http://127.0.0.1:8080/v1/whoami
{"data":{"client_ip":"203.0.113.9","peer":"127.0.0.1:59987","x_forwarded_for":"203.0.113.9","x_real_ip":"","proto":"HTTP/1.1"}}
```

`client_ip` is now the address from the header, because the hop that sent it
(`127.0.0.1`) is in `HTTP_TRUSTED_PROXIES`. With no header at all it falls back
to the peer:

```console
$ curl -s http://127.0.0.1:8080/v1/whoami
{"data":{"client_ip":"127.0.0.1","peer":"127.0.0.1:59988","x_forwarded_for":"","x_real_ip":"","proto":"HTTP/1.1"}}
```

Drop `127.0.0.0/8` from `HTTP_TRUSTED_PROXIES` and the first invocation goes
back to reporting `127.0.0.1`: an untrusted hop's header is not read.

`X-Real-IP` is never consulted in either mode.

## Three things that surprise people

**The trusted list is exhaustive, not additive.** Left empty, `xff` trusts
loopback and the private ranges — the usual sidecar or cluster-internal ingress
case. The moment you name a range, that implicit trust stands down: after
`HTTP_TRUSTED_PROXIES=10.4.0.0/16`, a header relayed from `127.0.0.1` is no
longer believed. Naming the load balancer is taken as a statement of where the
proxies are.

**Trusted proxies with the wrong source is a startup error, not a no-op:**

```console
$ HTTP_TRUSTED_PROXIES=10.4.0.0/16 go run ./examples/behind-proxy
ERROR  http server stopped  {"error": "build http server: trusted proxies are configured but the real ip source is \"peer\", which never reads forwarding headers: set HTTP_REAL_IP_SOURCE=xff or drop HTTP_TRUSTED_PROXIES"}
exit status 1
```

The ranges would never be consulted, and a silent no-op leaves you believing a
proxy is trusted when nothing reads its header.

**`xff` is only safe if the proxy overwrites the header.** Echo walks
`X-Forwarded-For` from the right and stops at the first hop that is not
trusted, so a client appending its own entry cannot win — but a proxy that
*appends to* rather than *replaces* an incoming header still hands you a chain
whose left end is attacker-controlled. Configure the proxy to overwrite.

## CORS

**With `HTTP_ALLLOWED_ORIGINS` unset, nothing is allowed cross-origin.** Every
preflight is answered without the header a browser needs, including one from
your own front end — which is the point: a deployment that has not named its
browser clients has not been asked to trust anybody.

With `HTTP_ALLLOWED_ORIGINS=https://zion.example`, a preflight from that origin
is answered with the headers a browser needs, and one from anywhere else is not:

```console
$ curl -s -i -X OPTIONS -H 'Origin: https://zion.example' \
       -H 'Access-Control-Request-Method: GET' http://127.0.0.1:8080/v1/whoami \
  | grep -iE '^HTTP|access-control-'
HTTP/1.1 204 No Content
Access-Control-Allow-Headers: Accept,Authorization,Content-Type,X-Request-Id
Access-Control-Allow-Methods: GET,POST,PUT,PATCH,DELETE
Access-Control-Allow-Origin: https://zion.example
Access-Control-Max-Age: 3600

$ curl -s -i -X OPTIONS -H 'Origin: https://evil.example' \
       -H 'Access-Control-Request-Method: GET' http://127.0.0.1:8080/v1/whoami \
  | grep -iE '^HTTP|access-control-'
HTTP/1.1 204 No Content
```

Note the second one is still a 204 — CORS is enforced by the browser refusing
the response, not by the server rejecting the request. The absence of the
`Access-Control-Allow-Origin` header is the enforcement. curl does not care,
which is exactly why testing CORS with curl misleads people.

`Access-Control-Max-Age: 3600` is the preflight cache: for the next hour that
browser skips the OPTIONS round trip for this route. `HTTP_CORS_MAX_AGE` moves
it, and `HTTP_CORS_MAX_AGE=0` removes the header entirely.

The request headers a preflight allows come from `HTTP_ALLOWED_HEADERS` — one
L, unlike the origins variable — and default to the four above. The allowed
**methods** are fixed at `GET, POST, PUT, PATCH, DELETE` and are not
configurable.

## Security headers

Every response carries them, without any variable being set:

```console
$ curl -s -i http://127.0.0.1:8080/v1/whoami | grep -iE 'x-content-type|x-frame|referrer|x-xss'
X-Xss-Protection: 0
X-Content-Type-Options: nosniff
X-Frame-Options: SAMEORIGIN
Referrer-Policy: strict-origin-when-cross-origin
```

`Strict-Transport-Security` is **not** among them. This process serves h2c in
the clear, so whether the origin it is reached at is HTTPS everywhere is a fact
about the load balancer in front, not about this server. Set `HTTP_HSTS_MAX_AGE`
when it is, and the header is sent for requests the proxy marked with
`X-Forwarded-Proto: https`.
