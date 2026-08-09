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

	// The messages the readiness log entries carry. As with requestLogMessage,
	// the detail lives in the structured fields; these are what an operator
	// greps for.
	readinessFailedMessage    = "readiness check failed"
	readinessRecoveredMessage = "readiness check recovered"
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

// readinessRegistry holds the checks a consumer registered, and what has
// already been said in the log about the ones that are failing. It is
// referenced by pointer from httpServer so that the value-receiver methods on
// that type share one registry rather than copying a lock.
type readinessRegistry struct {
	mu     sync.RWMutex
	checks []namedReadinessCheck
	// failures carries its own lock, which is never held together with mu.
	failures failureLog
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

// readinessNotice is one line a probe has decided is worth logging: a check
// that has started failing, one that is still failing and is due to be
// mentioned again, or one that has recovered.
type readinessNotice struct {
	name string
	// err is the failure being reported, and is nil on a recovery.
	err error
	// failingFor is how long the check had been failing when the notice was
	// produced. It is zero on the first report of a failure.
	failingFor time.Duration
}

// failureState is what the log already knows about one failing check.
type failureState struct {
	since      time.Time
	lastLogged time.Time
}

// failureLog decides what a probe says about its failures, and is the reason
// readiness logging does not drown the log it writes to.
//
// The naive rule — log every failing probe — is unusable here. Readiness is
// polled continuously by every load balancer and orchestrator in front of the
// instance, so a dependency that is down for an hour at a one second interval
// produces 3,600 identical lines per pod, and a fleet of twenty pods turns a
// single database outage into 72,000 lines that say one thing. That is the
// same flood the probe paths were taken out of the access log to avoid, and it
// buries the ok→fail line that actually carries the news.
//
// So notices are transitions, plus a repeat no more often than
// readinessLogInterval while a check stays down. The transition is the event;
// the repeat is what an operator needs when they start reading twenty minutes
// into an outage and the original line has already scrolled away, and it is
// what keeps a stuck dependency from being invisible in a log tail. Recovery
// is logged once, because an incident with no closing line has to be closed by
// guesswork.
//
// The cost of this over per-probe logging is that the log says how long a
// dependency has been down rather than how many probes observed it. Nobody
// counts probes; the readiness status itself is the signal for anything
// automated, and it is exported on the endpoint continuously either way.
type failureLog struct {
	mu sync.Mutex
	// failing holds one entry per check currently known to be down. Entries
	// are removed on recovery, so the map tracks broken dependencies rather
	// than growing with every check ever registered.
	failing map[string]failureState
}

// readinessLogInterval is the longest a check can stay down without being
// mentioned again.
const readinessLogInterval = time.Minute

// notices reports what should be logged for one probe's outcomes, in the order
// the checks were registered. now is passed in rather than read here so the
// decision can be tested at explicit points in time.
func (l *failureLog) notices(now time.Time, outcomes []outcome) []readinessNotice {
	l.mu.Lock()
	defer l.mu.Unlock()

	var notices []readinessNotice
	for _, o := range outcomes {
		state, known := l.failing[o.name]

		if o.err == nil {
			// Passing checks are silent unless this is the moment one stopped
			// failing. A line per passing check per probe would be the flood
			// this type exists to prevent.
			if known {
				delete(l.failing, o.name)
				notices = append(notices, readinessNotice{name: o.name, failingFor: now.Sub(state.since)})
			}
			continue
		}

		switch {
		case !known:
			state = failureState{since: now, lastLogged: now}
		case now.Sub(state.lastLogged) >= readinessLogInterval:
			state.lastLogged = now
		default:
			// Still down, and mentioned recently enough. The state stands as
			// it is, so the eventual repeat and the recovery both still know
			// when this started.
			continue
		}

		if l.failing == nil {
			l.failing = make(map[string]failureState, len(outcomes))
		}
		l.failing[o.name] = state
		notices = append(notices, readinessNotice{name: o.name, err: o.err, failingFor: now.Sub(state.since)})
	}

	return notices
}

// notices is the registry's view of the same decision, so callers reach for
// one object rather than two.
func (r *readinessRegistry) notices(now time.Time, outcomes []outcome) []readinessNotice {
	return r.failures.notices(now, outcomes)
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
	// rest of the API.
	//
	// Withholding it from the body does not withhold it from the operator. The
	// cause is written to the server log through the injected logger every
	// time, in both settings, as readinessFailedMessage with the check name and
	// the error text — see logReadiness. The body says which dependency is
	// down, which is what a prober needs; the log says why, where only someone
	// with access to the logs can read it.
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
// so a prober that hangs up takes its probe with it. Failures are logged by
// logReadiness; the response body never carries the cause unless Debug is set.
func (s httpServer) readyz(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), s.readinessTimeout())
	defer cancel()

	outcomes := s.readiness.run(ctx)
	s.logReadiness(outcomes)

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

// logReadiness writes what the response body will not: the error each failing
// dependency gave, through the injected logger, in the structured form the
// access log established.
//
// This is what makes withholding the cause from the body safe to default to.
// The readiness paths are excluded from the access log, so without these lines
// a failing check would leave no trace anywhere and the only way to learn why a
// pod was unready would be to redeploy it with HTTP_DEBUG on — which is to say,
// to reintroduce the leak to diagnose it.
//
// Failures are logged at warn rather than error. A dependency going away is an
// expected and usually self-healing condition — a database failing over, a
// rolling restart upstream, a pod that started before the thing it talks to —
// and this process is doing the right thing by answering 503 and standing down
// from the load balancer. Nothing here damaged a caller's request, which is
// what error level means in this package's access log. Logging routine
// dependency churn at error is how a team learns to filter error.
func (s httpServer) logReadiness(outcomes []outcome) {
	for _, n := range s.readiness.notices(time.Now(), outcomes) {
		if n.err == nil {
			s.logger.Infow(readinessRecoveredMessage,
				"check", n.name,
				"failing_for", n.failingFor.String(),
			)
			continue
		}

		s.logger.Warnw(readinessFailedMessage,
			"check", n.name,
			"error", n.err.Error(),
			"failing_for", n.failingFor.String(),
		)
	}
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
