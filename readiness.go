package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	readyzPath   = "/readyz"
	v1ReadyzPath = "/v1/readyz"

	// The two words a readiness body ever uses, for the report as a whole and
	// for each check in it.
	readyStatusOK   = "ok"
	readyStatusFail = "fail"
)

// ReadinessCheck reports whether one dependency is usable right now. A nil
// error means it is; any error means it is not, and the endpoint answers 503.
//
// The check is handed a context carrying the probe's deadline and must respect
// it: a check that blocks regardless is abandoned once the deadline passes and
// reported as failed, but its goroutine keeps running until it returns on its
// own. Prefer the context-aware call the driver already offers —
// sql.DB.PingContext, redis Ping with a context, an HTTP request built with
// http.NewRequestWithContext.
//
// A check runs on every probe, so it should be cheap: a ping, not a query.
type ReadinessCheck func(ctx context.Context) error

// namedReadinessCheck is a check together with the name it is reported under.
type namedReadinessCheck struct {
	name  string
	check ReadinessCheck
}

// readinessRegistry holds the checks a consumer registered. It is referenced
// by pointer from httpServer so that the value-receiver methods on that type
// share one registry rather than copying a lock.
type readinessRegistry struct {
	mu     sync.RWMutex
	checks []namedReadinessCheck
}

// register adds a check under name, refusing input that would produce a report
// nobody can read: an unnamed check, a check that is not there, or a second
// check claiming a name already taken.
func (r *readinessRegistry) register(name string, check ReadinessCheck) error {
	if name == "" {
		return fmt.Errorf("readiness check name is empty")
	}
	if check == nil {
		return fmt.Errorf("readiness check %q is nil", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.checks {
		if existing.name == name {
			return fmt.Errorf("readiness check %q is already registered", name)
		}
	}

	r.checks = append(r.checks, namedReadinessCheck{name: name, check: check})
	return nil
}

// snapshot copies the registered checks so a probe can run without holding the
// lock across calls into consumer code.
func (r *readinessRegistry) snapshot() []namedReadinessCheck {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]namedReadinessCheck(nil), r.checks...)
}

// outcome is what one check did on one probe.
type outcome struct {
	name string
	err  error
}

// run executes every registered check against ctx and returns their outcomes
// in registration order, so the report reads the same way on every probe.
//
// The checks run concurrently: serially, one slow dependency would push the
// probe past its deadline and make every dependency behind it look broken.
// Collection stops when ctx is done, and checks that had not answered by then
// are reported as failed with the context's error — a probe must never wait on
// a dependency that has stopped answering, which is the condition it exists to
// detect.
func (r *readinessRegistry) run(ctx context.Context) []outcome {
	checks := r.snapshot()

	outcomes := make([]outcome, len(checks))
	answered := make([]bool, len(checks))
	for i, c := range checks {
		outcomes[i].name = c.name
	}

	type indexed struct {
		index int
		err   error
	}
	// Buffered for every check, so a goroutine whose result arrives after the
	// deadline still sends and exits instead of blocking forever.
	results := make(chan indexed, len(checks))
	for i, c := range checks {
		go func(i int, c namedReadinessCheck) {
			results <- indexed{index: i, err: runReadinessCheck(ctx, c)}
		}(i, c)
	}

	for remaining := len(checks); remaining > 0; remaining-- {
		select {
		case res := <-results:
			outcomes[res.index].err = res.err
			answered[res.index] = true
		case <-ctx.Done():
			for i := range outcomes {
				if !answered[i] {
					outcomes[i].err = ctx.Err()
				}
			}
			return outcomes
		}
	}

	return outcomes
}

// runReadinessCheck calls one check and converts a panic into a failed check.
// The checks run on their own goroutines, where echo's Recover middleware
// cannot reach them, so a consumer's nil map or nil pointer would otherwise
// take the whole process down from a probe.
func runReadinessCheck(ctx context.Context, c namedReadinessCheck) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("readiness check panicked: %v", r)
		}
	}()

	return c.check(ctx)
}

// readinessReport is the readiness response body.
type readinessReport struct {
	Status string            `json:"status"`
	Checks []readinessResult `json:"checks"`
}

// readinessResult is one check's line in that body.
type readinessResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Error carries the dependency's own error text. It is populated only when
	// Debug is set, because that text is the dependency's diagnostic and not a
	// message for whoever reached the endpoint: a driver failure names hosts,
	// ports, database and account — `pq: password authentication failed for
	// user "zion_admin"` — and readiness is routinely exposed further than the
	// rest of the API. The name and status alone say which dependency is down,
	// which is what a prober or an on-call engineer needs from the response;
	// the cause belongs in the logs.
	Error string `json:"error,omitempty"`
}

// RegisterReadinessCheck registers a dependency check under name, to be run by
// the readiness endpoints. It returns an error if the name is empty, the check
// is nil, or the name is already taken.
//
// Registration is safe to call at any point: before Run, and while the server
// is serving. The registry is guarded by its own lock and each probe works
// from a snapshot, so a check registered mid-flight is picked up by the next
// probe and never by one already in progress.
//
//	s, err := server.NewHTTP(nil, log)
//	...
//	if err := s.RegisterReadinessCheck("postgres", db.PingContext); err != nil {
//	    return err
//	}
//
// Checks report on dependencies, not on the process: liveness at /healthz and
// /v1/healthz stays a static 200 no matter what they say, so a failing
// dependency withdraws this instance from load balancing without also telling
// an orchestrator to restart it.
func (s *httpServer) RegisterReadinessCheck(name string, check ReadinessCheck) error {
	return s.readiness.register(name, check)
}

// readyz answers a readiness probe: 200 when every registered check passes,
// 503 when any of them fails, and 200 when none are registered — nothing has
// declared a dependency, so there is nothing to be unready for.
//
// The checks run under the request's own context bounded by readinessTimeout,
// so a prober that hangs up takes its probe with it.
func (s httpServer) readyz(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), s.readinessTimeout())
	defer cancel()

	outcomes := s.readiness.run(ctx)

	report := readinessReport{
		Status: readyStatusOK,
		// Built with make so that a server with no checks reports an empty
		// list rather than a null.
		Checks: make([]readinessResult, 0, len(outcomes)),
	}

	status := http.StatusOK
	for _, o := range outcomes {
		result := readinessResult{Name: o.name, Status: readyStatusOK}
		if o.err != nil {
			result.Status = readyStatusFail
			report.Status = readyStatusFail
			status = http.StatusServiceUnavailable

			if s.config.Debug {
				result.Error = o.err.Error()
			}
		}
		report.Checks = append(report.Checks, result)
	}

	return c.JSON(status, report)
}

// readinessTimeout is the bound on one probe. A non-positive value means the
// config was built by hand without one, and using it as-is would expire the
// context before any check ran and report every dependency as down, so fall
// back to the package default.
func (s httpServer) readinessTimeout() time.Duration {
	if s.config.ReadinessTimeout <= 0 {
		return defaultReadinessTimeout
	}
	return s.config.ReadinessTimeout
}
