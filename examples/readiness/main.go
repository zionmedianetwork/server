// Command readiness demonstrates the split between liveness and readiness.
//
// Two stub dependencies are registered as readiness checks and can be knocked
// over at runtime through a debug route, so you can watch /readyz turn 503 and
// name the dependency that failed while /healthz keeps answering 200 — which is
// the whole point of the split: a database outage must withdraw the instance
// from load balancing without telling an orchestrator to restart it.
//
// Run it with:
//
//	HTTP_BIND_ADDRESS=:8080 HTTP_READINESS_TIMEOUT=2s go run ./examples/readiness
//
// See the README next to this file for the requests to make against it.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/zionmedianetwork/logam"
	"github.com/zionmedianetwork/server"
)

// dependency stands in for something this service needs in order to do its job:
// a database handle, a cache client, an object store. Nothing here talks to a
// real one — no driver is imported, because the shape of the check is the
// subject, not the driver.
type dependency struct {
	// up is flipped at runtime by the debug route below, so a reader can watch
	// readiness change. It is atomic because a probe reads it from the check's
	// own goroutine while a request handler writes it. A real check has nothing
	// like this: it asks the dependency itself, on every probe.
	up atomic.Bool
	// latency simulates the round trip a ping costs.
	latency time.Duration
	// failure is the error reported while the dependency is down. It is written
	// to look like a driver's, because that is what makes such text unfit for a
	// response body: hosts, ports and account names.
	failure string
}

// newDependency returns a dependency that starts out healthy.
func newDependency(latency time.Duration, failure string) *dependency {
	d := &dependency{latency: latency, failure: failure}
	d.up.Store(true)
	return d
}

// ping is a server.ReadinessCheck.
//
// It respects the context it is handed, which is what a check has to do: the
// probe abandons a check that overruns HTTP_READINESS_TIMEOUT and reports it as
// failed, but the goroutine behind it keeps running until it returns on its own.
// In real code this is sql.DB.PingContext, a redis Ping with a context, or an
// http.NewRequestWithContext — never a call that cannot be cancelled.
func (d *dependency) ping(ctx context.Context) error {
	timer := time.NewTimer(d.latency)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
		return fmt.Errorf("ping abandoned: %w", ctx.Err())
	}

	if !d.up.Load() {
		return errors.New(d.failure)
	}
	return nil
}

// namedDependency pairs a dependency with the name it is reported under. A
// slice rather than a map, because checks appear in the readiness body in
// registration order and a map would reshuffle the report on every probe.
type namedDependency struct {
	name string
	dep  *dependency
}

func main() {
	// This example logs through github.com/zionmedianetwork/logam, and it is
	// the one that shows the compatibility path. logam's Logger carries
	// seventeen methods; server.Logger asks for three of them, and Go satisfies
	// interfaces structurally, so this value is accepted as it is — no adapter,
	// and no change to code written before that interface existed. logam is one
	// option among several: ../minimal runs the same package on log/slog from
	// the standard library and imports nothing else.
	log := logam.NewLogger(logam.Config{
		LogLevel:    "info",
		LogFormat:   "console",
		Environment: logam.EnvDevelopment,
	})

	if err := serve(log); err != nil {
		log.Errorw("http server stopped", "error", err)
		os.Exit(1)
	}
}

func serve(log logam.Logger) error {
	s, err := server.NewHTTP(nil, log)
	if err != nil {
		return fmt.Errorf("build http server: %w", err)
	}

	deps := []namedDependency{
		{
			name: "postgres",
			dep: newDependency(
				5*time.Millisecond,
				`pq: dial tcp 10.0.0.7:5432: connect: connection refused (user "zion_admin")`,
			),
		},
		{
			name: "redis",
			dep:  newDependency(2*time.Millisecond, "redis: dial tcp 10.0.0.8:6379: i/o timeout"),
		},
		{
			// A third check that is slower than the other two together. The
			// probe runs every check concurrently, so the probe costs about as
			// long as the slowest one rather than the sum.
			name: "object-store",
			dep:  newDependency(40*time.Millisecond, "s3: RequestTimeout: request to zion-media-eu timed out"),
		},
	}

	byName := make(map[string]*dependency, len(deps))
	for _, d := range deps {
		// Registration is refused for an empty name, a nil check, or a name
		// already taken. Each of those would produce a report nobody can read,
		// so handle the error rather than discarding it.
		if err := s.RegisterReadinessCheck(d.name, d.dep.ping); err != nil {
			return fmt.Errorf("register readiness check %q: %w", d.name, err)
		}
		byName[d.name] = d.dep
	}

	e := s.Echo()
	// An ordinary application route. It keeps answering while readiness is
	// failing: readiness tells the load balancer to stop sending new traffic,
	// it does not make the process refuse the traffic it still receives.
	e.GET("/v1/videos", listVideos)
	// The toggle. This exists so the behaviour can be exercised with curl and
	// has no business in a real service — anything that can make an instance
	// unready from the outside is a denial of service with a nice URL.
	e.POST("/debug/dependencies/:name/:state", toggleDependency(byName))

	log.Infow("http server starting",
		"liveness", "/healthz, /v1/healthz",
		"readiness", "/readyz, /v1/readyz",
		"checks", len(deps),
	)

	return s.Run()
}

// listVideos is the application traffic, here only so there is something to
// watch keep working while a dependency is down.
func listVideos(c echo.Context) error {
	return server.HTTPResponse(c, []string{"episode-1", "episode-2"})
}

// toggleDependency knocks a stub dependency over and stands it back up:
//
//	POST /debug/dependencies/postgres/down
//	POST /debug/dependencies/postgres/up
func toggleDependency(byName map[string]*dependency) echo.HandlerFunc {
	return func(c echo.Context) error {
		name := c.Param("name")
		dep, ok := byName[name]
		if !ok {
			return echo.NewHTTPError(http.StatusNotFound, "unknown dependency "+name)
		}

		state := c.Param("state")
		var up bool
		switch state {
		case "up":
			up = true
		case "down":
			up = false
		default:
			return echo.NewHTTPError(http.StatusBadRequest, `state must be "up" or "down"`)
		}

		dep.up.Store(up)

		// Confirmation is answered 200 and unwrapped: {"message":"..."}.
		return server.HTTPResponse(c, server.Confirmation{
			Message: fmt.Sprintf("dependency %q is now %s", name, state),
		})
	}
}
