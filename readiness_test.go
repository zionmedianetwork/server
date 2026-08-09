package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// readinessPaths are the two endpoints the checks answer on, kept together so
// every assertion is made against both.
var readinessPaths = []string{readyzPath, v1ReadyzPath}

// passingCheck is a dependency that is up.
func passingCheck(context.Context) error { return nil }

// failingCheck is a dependency that is down, and whose error is the kind a
// driver actually produces: it names the account it tried to authenticate as.
// dsnError is shared with the debug tests, because this is the same leak.
func failingCheck(context.Context) error { return dsnError }

// register adds a check and fails the test if the registration was refused.
func register(t *testing.T, s *httpServer, name string, check ReadinessCheck) {
	t.Helper()

	if err := s.RegisterReadinessCheck(name, check); err != nil {
		t.Fatalf("RegisterReadinessCheck(%q) error = %v, want nil", name, err)
	}
}

// probeReadiness sends one readiness request and returns the recorder.
func probeReadiness(t *testing.T, s *httpServer, path string) *httptest.ResponseRecorder {
	t.Helper()

	return do(t, s, httptest.NewRequest(http.MethodGet, path, nil))
}

// decodeReport parses a readiness body, which every one of these tests asserts
// on directly: the point of the endpoint is what it says to its caller.
func decodeReport(t *testing.T, rec *httptest.ResponseRecorder) readinessReport {
	t.Helper()

	var report readinessReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("readiness body is not a report: %v (%s)", err, rec.Body.String())
	}
	return report
}

// checkResult returns the reported line for a named check.
func checkResult(t *testing.T, report readinessReport, name string) readinessResult {
	t.Helper()

	for _, result := range report.Checks {
		if result.Name == name {
			return result
		}
	}
	t.Fatalf("check %q missing from the readiness report: %+v", name, report.Checks)
	return readinessResult{}
}

func TestReadinessIsOKWhenEveryCheckPasses(t *testing.T) {
	for _, path := range readinessPaths {
		t.Run(path, func(t *testing.T) {
			s, _ := newTestServer(t)
			register(t, s, "postgres", passingCheck)
			register(t, s, "cache", passingCheck)

			rec := probeReadiness(t, s, path)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if body := compactJSON(t, rec.Body.String()); body != `{"status":"ok","checks":[{"name":"postgres","status":"ok"},{"name":"cache","status":"ok"}]}` {
				t.Errorf("body = %s, want both checks reported as ok in registration order", body)
			}
		})
	}
}

// TestReadinessWithNoChecksIsReady pins the behaviour of the endpoint before
// any consumer has registered anything: nothing has declared a dependency, so
// there is nothing to be unready for. The empty list is reported as a list
// rather than a null so a consumer can iterate it without checking.
func TestReadinessWithNoChecksIsReady(t *testing.T) {
	s, _ := newTestServer(t)

	rec := probeReadiness(t, s, readyzPath)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := compactJSON(t, rec.Body.String()); body != `{"status":"ok","checks":[]}` {
		t.Errorf("body = %s, want an empty check list", body)
	}
}

// TestReadinessFailsAndNamesTheFailingCheck covers M4. A static 200 reported a
// pod as healthy while its database was unreachable, so Kubernetes kept routing
// traffic to it; the report has to say which dependency is down, or an operator
// has nothing to act on.
func TestReadinessFailsAndNamesTheFailingCheck(t *testing.T) {
	for _, path := range readinessPaths {
		t.Run(path, func(t *testing.T) {
			s, _ := newTestServer(t)
			register(t, s, "postgres", failingCheck)
			register(t, s, "cache", passingCheck)

			rec := probeReadiness(t, s, path)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
			}

			report := decodeReport(t, rec)
			if report.Status != readyStatusFail {
				t.Errorf("status = %q, want %q", report.Status, readyStatusFail)
			}
			if got := checkResult(t, report, "postgres").Status; got != readyStatusFail {
				t.Errorf("postgres status = %q, want %q", got, readyStatusFail)
			}
			// The dependency that is fine must still be reported as fine, or
			// the report cannot be used to tell them apart.
			if got := checkResult(t, report, "cache").Status; got != readyStatusOK {
				t.Errorf("cache status = %q, want %q", got, readyStatusOK)
			}
		})
	}
}

// TestReadinessDoesNotLeakDependencyErrors is the security half of M4, and the
// same class of leak the previous change removed by defaulting e.Debug to
// false. Readiness is routinely exposed further than the rest of the API — a
// probe endpoint on an ingress, a debug page, a status dashboard — and a body
// echoing the driver's error hands over hostnames, ports and account names to
// anyone who can reach it.
func TestReadinessDoesNotLeakDependencyErrors(t *testing.T) {
	s, _ := newTestServer(t)
	register(t, s, "postgres", failingCheck)

	rec := probeReadiness(t, s, readyzPath)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	body := compactJSON(t, rec.Body.String())
	if want := `{"status":"fail","checks":[{"name":"postgres","status":"fail"}]}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	if strings.Contains(body, "zion_admin") || strings.Contains(body, "pq:") {
		t.Errorf("readiness body leaks the dependency error to the client: %s", body)
	}
	if got := checkResult(t, decodeReport(t, rec), "postgres").Error; got != "" {
		t.Errorf("check error = %q, want it withheld with Debug off", got)
	}
}

// TestReadinessIncludesDependencyErrorsWhenDebugEnabled proves the detail is
// withheld rather than discarded, so a deployment that opts in still gets the
// cause on the endpoint.
func TestReadinessIncludesDependencyErrorsWhenDebugEnabled(t *testing.T) {
	cfg := testConfig()
	cfg.Debug = true

	s, _ := newTestServerWith(t, cfg)
	register(t, s, "postgres", failingCheck)
	register(t, s, "cache", passingCheck)

	rec := probeReadiness(t, s, readyzPath)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	body := compactJSON(t, rec.Body.String())
	if !strings.Contains(body, "zion_admin") {
		t.Errorf("body = %s, want it to carry the dependency error with Debug on", body)
	}

	report := decodeReport(t, rec)
	if got := checkResult(t, report, "postgres").Error; got != dsnError.Error() {
		t.Errorf("check error = %q, want %q", got, dsnError.Error())
	}
	// A check that passed has no error to report in either setting.
	if got := checkResult(t, report, "cache").Error; got != "" {
		t.Errorf("passing check error = %q, want it absent", got)
	}
}

// TestReadinessBoundsAHangingCheck covers the failure the timeout exists for: a
// dependency that has stopped answering is exactly what readiness is asked
// about, and a probe that blocks on it tells the prober nothing until the
// prober's own timeout fires. The check here ignores its context, which is the
// worst case, so the bound has to be enforced by the probe rather than by the
// check's cooperation.
func TestReadinessBoundsAHangingCheck(t *testing.T) {
	const readinessTimeout = 50 * time.Millisecond

	cfg := testConfig()
	cfg.ReadinessTimeout = readinessTimeout

	s, _ := newTestServerWith(t, cfg)

	release := make(chan struct{})
	// Never leave the check parked, even if an assertion below fails.
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	register(t, s, "wedged", func(context.Context) error {
		<-release
		return nil
	})
	register(t, s, "cache", passingCheck)

	start := time.Now()
	rec := probeReadiness(t, s, readyzPath)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	// Generous, because the assertion is "bounded", not "prompt": a factor of
	// ten still fails a probe that waits on the check.
	if limit := 10 * readinessTimeout; elapsed > limit {
		t.Errorf("probe took %s, want it bounded near %s", elapsed, readinessTimeout)
	}

	report := decodeReport(t, rec)
	if got := checkResult(t, report, "wedged").Status; got != readyStatusFail {
		t.Errorf("wedged check status = %q, want %q", got, readyStatusFail)
	}
	// The check that did answer is reported on its own merits rather than
	// being lost with the one that did not.
	if got := checkResult(t, report, "cache").Status; got != readyStatusOK {
		t.Errorf("cache status = %q, want %q", got, readyStatusOK)
	}
}

// TestReadinessRunsChecksConcurrently proves the checks do not queue behind one
// another. Each check blocks until all of them have started, so the probe can
// only complete if they run at the same time; run serially the first would sit
// there until the deadline and the probe would answer 503.
func TestReadinessRunsChecksConcurrently(t *testing.T) {
	const checkCount = 3

	cfg := testConfig()
	cfg.ReadinessTimeout = 500 * time.Millisecond

	s, _ := newTestServerWith(t, cfg)

	var started sync.WaitGroup
	started.Add(checkCount)
	allStarted := make(chan struct{})
	go func() {
		started.Wait()
		close(allStarted)
	}()

	barrier := func(ctx context.Context) error {
		started.Done()
		select {
		case <-allStarted:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	for i := 0; i < checkCount; i++ {
		register(t, s, "dependency-"+strconv.Itoa(i), barrier)
	}

	rec := probeReadiness(t, s, readyzPath)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: the checks did not overlap (%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestLivenessStaysOKWhileReadinessFails is the whole point of the split. A
// process whose database is unreachable is not ready for traffic, but it is
// alive: a liveness probe that failed with it would have the orchestrator
// restart a pod that has nothing wrong with it, turning a dependency outage
// into a crash loop.
func TestLivenessStaysOKWhileReadinessFails(t *testing.T) {
	s, _ := newTestServer(t)
	register(t, s, "postgres", failingCheck)

	for _, path := range []string{healthzPath, v1HealthzPath} {
		rec := do(t, s, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want %d while a readiness check is failing", path, rec.Code, http.StatusOK)
		}
		if got := rec.Body.String(); got != http.StatusText(http.StatusOK) {
			t.Errorf("%s body = %q, want %q", path, got, http.StatusText(http.StatusOK))
		}
	}

	for _, path := range readinessPaths {
		if rec := probeReadiness(t, s, path); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want %d", path, rec.Code, http.StatusServiceUnavailable)
		}
	}
}

// TestRequestLoggerSkipsReadinessPaths keeps probe traffic out of the log for
// the same reason the liveness paths are skipped, and more so: readiness is
// polled by every load balancer that fronts the instance.
func TestRequestLoggerSkipsReadinessPaths(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "readyz is skipped", target: readyzPath},
		{name: "versioned readyz is skipped", target: v1ReadyzPath},
		{name: "readyz with trailing slash is skipped", target: readyzPath + "/"},
		{name: "readyz with query string is skipped", target: readyzPath + "?probe=readiness"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, log := newTestServer(t)
			// A failing check answers 503, which is the status the logger
			// would otherwise be most eager to record.
			register(t, s, "postgres", failingCheck)

			do(t, s, httptest.NewRequest(http.MethodGet, tt.target, nil))

			if entries := log.recorded(); len(entries) != 0 {
				t.Errorf("logged %d entries for a readiness probe, want 0: %+v", len(entries), entries)
			}
		})
	}
}

// TestReadinessCheckPanicIsContained keeps a probe from killing the process.
// The checks run on their own goroutines, out of reach of echo's Recover
// middleware, so an unguarded panic in a consumer's check would take the whole
// server down every time Kubernetes polled it.
func TestReadinessCheckPanicIsContained(t *testing.T) {
	cfg := testConfig()
	cfg.Debug = true

	s, _ := newTestServerWith(t, cfg)
	register(t, s, "exploding", func(context.Context) error {
		panic("nil map write")
	})

	rec := probeReadiness(t, s, readyzPath)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := checkResult(t, decodeReport(t, rec), "exploding").Error; !strings.Contains(got, "nil map write") {
		t.Errorf("check error = %q, want it to report the panic", got)
	}
}

// TestReadinessCheckObservesTheProbeDeadline pins the contract the check is
// handed: a context that already carries the probe's deadline, so a
// context-aware driver call is cut off with the probe rather than after it.
func TestReadinessCheckObservesTheProbeDeadline(t *testing.T) {
	cfg := testConfig()
	cfg.ReadinessTimeout = 250 * time.Millisecond

	s, _ := newTestServerWith(t, cfg)

	deadlines := make(chan time.Time, 1)
	register(t, s, "postgres", func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadlines <- time.Time{}
			return nil
		}
		deadlines <- deadline
		return nil
	})

	probeReadiness(t, s, readyzPath)

	deadline := <-deadlines
	if deadline.IsZero() {
		t.Fatal("readiness check ran without a deadline, want the probe's")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > cfg.ReadinessTimeout {
		t.Errorf("check deadline is %s away, want it within the %s probe bound", remaining, cfg.ReadinessTimeout)
	}
}

// TestRegisterReadinessCheckRejectsBadRegistrations covers the wiring mistakes
// that would otherwise produce a report nobody can read.
func TestRegisterReadinessCheckRejectsBadRegistrations(t *testing.T) {
	t.Run("empty name", func(t *testing.T) {
		s, _ := newTestServer(t)

		if err := s.RegisterReadinessCheck("", passingCheck); err == nil {
			t.Error("RegisterReadinessCheck() error = nil, want a rejection of the empty name")
		}
	})

	t.Run("nil check", func(t *testing.T) {
		s, _ := newTestServer(t)

		err := s.RegisterReadinessCheck("postgres", nil)
		if err == nil {
			t.Fatal("RegisterReadinessCheck() error = nil, want a rejection of the nil check")
		}
		if !strings.Contains(err.Error(), "postgres") {
			t.Errorf("RegisterReadinessCheck() error = %v, want it to name the check", err)
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		s, _ := newTestServer(t)
		register(t, s, "postgres", passingCheck)

		// Refused rather than replacing the first: two checks under one name
		// means one of them is a copy-paste mistake, and silently keeping
		// either would hide a dependency nobody is probing.
		err := s.RegisterReadinessCheck("postgres", failingCheck)
		if err == nil {
			t.Fatal("RegisterReadinessCheck() error = nil, want a rejection of the duplicate name")
		}
		if !strings.Contains(err.Error(), "postgres") {
			t.Errorf("RegisterReadinessCheck() error = %v, want it to name the check", err)
		}

		// The rejected registration must not have been applied either.
		if rec := probeReadiness(t, s, readyzPath); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d: the refused check was registered anyway", rec.Code, http.StatusOK)
		}
	})
}

// TestRegisterReadinessCheckIsSafeDuringServing pins the concurrency contract:
// registration may happen while the server is answering probes. Under -race
// this is the assertion that the registry is properly guarded; the status
// assertions are there so the test also fails if a probe misses or double
// counts a check.
func TestRegisterReadinessCheckIsSafeDuringServing(t *testing.T) {
	const registrations = 16

	s, _, baseURL := listenTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.run(ctx, noopStop) }()

	waitForServing(t, baseURL)

	var wg sync.WaitGroup
	wg.Add(2 * registrations)

	for i := 0; i < registrations; i++ {
		go func(i int) {
			defer wg.Done()
			if err := s.RegisterReadinessCheck("dependency-"+strconv.Itoa(i), passingCheck); err != nil {
				t.Errorf("RegisterReadinessCheck() error = %v, want nil", err)
			}
		}(i)

		go func() {
			defer wg.Done()
			resp, err := testClient().Get(baseURL + readyzPath)
			if err != nil {
				t.Errorf("readiness probe error = %v, want nil", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("readiness status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
		}()
	}

	wg.Wait()

	// Every registration landed exactly once.
	report := decodeReport(t, probeReadiness(t, s, readyzPath))
	if len(report.Checks) != registrations {
		t.Errorf("readiness reports %d checks, want %d: %+v", len(report.Checks), registrations, report.Checks)
	}

	cancel()
	if err := awaitRun(t, errCh); err != nil {
		t.Errorf("run() error = %v, want nil", err)
	}
}

func TestReadinessTimeoutFallsBackToDefault(t *testing.T) {
	tests := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "unset falls back", configured: 0, want: defaultReadinessTimeout},
		{name: "negative falls back", configured: -time.Second, want: defaultReadinessTimeout},
		{name: "configured value is used", configured: 3 * time.Second, want: 3 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			s.config.ReadinessTimeout = tt.configured

			if got := s.readinessTimeout(); got != tt.want {
				t.Errorf("readinessTimeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNewHttpConfigReadinessTimeout(t *testing.T) {
	t.Run("defaults to two seconds", func(t *testing.T) {
		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.ReadinessTimeout != defaultReadinessTimeout {
			t.Errorf("ReadinessTimeout = %s, want %s", cfg.ReadinessTimeout, defaultReadinessTimeout)
		}
	})

	t.Run("reads HTTP_READINESS_TIMEOUT", func(t *testing.T) {
		t.Setenv("HTTP_READINESS_TIMEOUT", "750ms")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if want := 750 * time.Millisecond; cfg.ReadinessTimeout != want {
			t.Errorf("ReadinessTimeout = %s, want %s", cfg.ReadinessTimeout, want)
		}
	})
}
