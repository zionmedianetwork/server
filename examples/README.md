# Examples

Four runnable programs, each in its own directory. They are part of this
module, so `go build ./...`, `go vet ./...`, `gofmt -l .` and
`golangci-lint run ./...` at the repository root cover them, and none of them
adds a dependency — the stub database, cache and object store are plain structs.

| Example | What it demonstrates | Logger |
| --- | --- | --- |
| [`minimal`](minimal) | The smallest correct service: config from the environment, two routes, both response shapes, and the call site that handles `Run()`'s error and exits non-zero. Copy this one. | `log/slog` |
| [`readiness`](readiness) | Readiness checks over stub dependencies you can knock over with curl, while liveness keeps answering 200. | `logam` |
| [`behind-proxy`](behind-proxy) | Production-shaped configuration behind a TLS-terminating load balancer, with a route that shows spoofed `X-Forwarded-For` being ignored or honoured. | `logam` |
| [`streaming`](streaming) | A request timeout tight enough for the API but exempting a streaming route — plus the `HTTP_WRITETIMEOUT` interaction that makes or breaks it. | `log/slog` |

## Two logging approaches, two examples each

`NewHTTP` takes a `server.Logger`: `Infow`, `Warnw`, `Errorw`, and nothing else.
The set is split so both ways of satisfying that are on show.

- **`minimal` and `streaming` use `log/slog`** through a three-line adapter
  written out at the top of each `main.go`. Neither imports
  `github.com/zionmedianetwork/logam`, or any logging library: `log/slog` is
  standard library, so those two programs prove this package runs with no
  logging dependency whatsoever.
- **`readiness` and `behind-proxy` use `logam.NewLogger(...)`** and pass the
  result to `NewHTTP` directly. `logam.Logger` has seventeen methods and
  satisfies the three-method interface structurally, so no adapter and no
  wrapper is involved, and code written before the interface existed compiles
  unchanged. logam is one option among several here, not a requirement.

The choice changes nothing about what is logged: the access log entries and the
readiness lines carry the same fields in every example, and only their rendering
— slog JSON against logam's console format — differs between the transcripts in
each directory's README.

Run any of them from the repository root:

```bash
HTTP_BIND_ADDRESS=127.0.0.1:8080 go run ./examples/minimal
```

Each directory's README lists the environment variables it expects and the
requests to make against it.
