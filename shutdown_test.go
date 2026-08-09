package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	// runTimeout bounds every wait on the server lifecycle so a regression
	// fails the test instead of hanging the test binary.
	runTimeout = 15 * time.Second
	// pollInterval is how often the readiness helpers retry.
	pollInterval = 5 * time.Millisecond
)

// noopStop stands in for the signal.NotifyContext stop function when a test
// drives the shutdown with a plain context instead of a real signal.
func noopStop() {}

// listenTestServer builds a server on testConfig whose listener is created up
// front, so the test knows the address without racing startup and a port
// collision fails here rather than inside the serve goroutine. It returns the
// server, its logger, and the base URL to talk to it.
func listenTestServer(t *testing.T) (*httpServer, *stubLogger, string) {
	t.Helper()

	return listenTestServerWith(t, testConfig())
}

// listenTestServerWith is listenTestServer against a config the test chose,
// for the cases that have to be exercised over a real connection rather than
// through the in-process handler.
func listenTestServerWith(t *testing.T, cfg *HttpConfig) (*httpServer, *stubLogger, string) {
	t.Helper()

	s, log := newTestServerWith(t, cfg)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	e := s.Echo()
	e.Listener = l
	// Keep echo's startup line out of the test output.
	e.HidePort = true

	s.config.BindAddress = l.Addr().String()
	s.config.ShutdownTimeout = runTimeout

	return s, log, "http://" + l.Addr().String()
}

// testClient returns a client that dials a fresh connection per request, so a
// closed listener is observable rather than masked by a pooled connection.
func testClient() *http.Client {
	return &http.Client{
		Timeout:   runTimeout,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

// waitForServing blocks until the server answers a health check. Health checks
// are excluded from request logging, so this leaves no entries behind.
func waitForServing(t *testing.T, baseURL string) {
	t.Helper()

	client := testClient()
	deadline := time.Now().Add(runTimeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + healthzPath)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("server at %s never became ready within %s", baseURL, runTimeout)
}

// waitForListenerClosed blocks until a new connection is refused, which proves
// the shutdown has actually begun rather than merely been requested.
func waitForListenerClosed(t *testing.T, baseURL string) {
	t.Helper()

	client := testClient()
	deadline := time.Now().Add(runTimeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + healthzPath)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		time.Sleep(pollInterval)
	}
	t.Fatalf("listener at %s was still accepting connections after %s", baseURL, runTimeout)
}

// awaitRun waits for run to return and yields its error.
func awaitRun(t *testing.T, errCh <-chan error) error {
	t.Helper()

	select {
	case err := <-errCh:
		return err
	case <-time.After(runTimeout):
		t.Fatalf("run() did not return within %s", runTimeout)
		return nil
	}
}

// TestRunShutdownIsGraceful covers C1: http.ErrServerClosed is the success
// signal Shutdown produces, and the original defect read it as a failure. It
// reached logger.Fatal, which calls os.Exit(1) in the real logger, so the
// process died at the exact moment it was supposed to be draining.
//
// That half of C1 is now a property of the type rather than of this test: the
// package takes a server.Logger, which declares Infow, Warnw and Errorw and no
// Fatal, so there is no longer a method to reach. Reintroducing the defect
// would fail to compile.
//
// What is still only true by construction, and so is what the assertions below
// pin down, is that a graceful shutdown is reported as success: run returns nil
// and writes nothing at error level. Both halves matter to a caller. A non-nil
// return from an ordinary SIGTERM makes a main() that exits on error report a
// clean stop as a crash, and a spurious error entry does the same to anything
// watching the logs.
func TestRunShutdownIsGraceful(t *testing.T) {
	s, log, baseURL := listenTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.run(ctx, noopStop) }()

	waitForServing(t, baseURL)
	cancel()

	if err := awaitRun(t, errCh); err != nil {
		t.Errorf("run() error = %v, want nil on a graceful shutdown", err)
	}
	if errs := log.entriesAt(levelError); len(errs) != 0 {
		t.Errorf("logged %d error entries on a clean shutdown, want 0: %+v", len(errs), errs)
	}
}

// TestRunStopsSignalHandlingOnceDrainingStarts covers the second half of C2:
// the stop function must be invoked as soon as the drain begins. That restores
// the default signal disposition, so a second SIGINT or SIGTERM aborts a stuck
// drain instead of being swallowed.
func TestRunStopsSignalHandlingOnceDrainingStarts(t *testing.T) {
	s, _, baseURL := listenTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- s.run(ctx, func() { close(stopped) }) }()

	waitForServing(t, baseURL)
	cancel()

	select {
	case <-stopped:
	case <-time.After(runTimeout):
		t.Fatalf("run() did not release the signal handlers within %s", runTimeout)
	}

	if err := awaitRun(t, errCh); err != nil {
		t.Errorf("run() error = %v, want nil", err)
	}
}

// TestRunCompletesInFlightRequestDuringDrain covers the consequence of C1: the
// old code exited the process the instant draining began, severing live
// requests. The handler is released only after the listener has closed, so the
// request is provably mid-flight when the drain starts.
func TestRunCompletesInFlightRequestDuringDrain(t *testing.T) {
	s, _, baseURL := listenTestServer(t)

	const body = "drained"

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	// Never leave the handler parked, even if an assertion below fails.
	t.Cleanup(releaseNow)

	s.Echo().GET("/slow", func(c echo.Context) error {
		close(started)
		<-release
		return c.String(http.StatusOK, body)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.run(ctx, noopStop) }()

	waitForServing(t, baseURL)

	type response struct {
		status int
		body   string
		err    error
	}
	respCh := make(chan response, 1)
	go func() {
		resp, err := testClient().Get(baseURL + "/slow")
		if err != nil {
			respCh <- response{err: err}
			return
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		respCh <- response{status: resp.StatusCode, body: string(payload), err: err}
	}()

	<-started
	cancel()
	waitForListenerClosed(t, baseURL)
	releaseNow()

	select {
	case got := <-respCh:
		if got.err != nil {
			t.Fatalf("in-flight request failed during drain: %v", got.err)
		}
		if got.status != http.StatusOK {
			t.Errorf("in-flight status = %d, want %d", got.status, http.StatusOK)
		}
		if got.body != body {
			t.Errorf("in-flight body = %q, want %q", got.body, body)
		}
	case <-time.After(runTimeout):
		t.Fatalf("in-flight request did not complete within %s", runTimeout)
	}

	if err := awaitRun(t, errCh); err != nil {
		t.Errorf("run() error = %v, want nil", err)
	}
}

// TestRunReturnsShutdownErrorWhenGracePeriodExpires proves the configured
// grace period is the one actually applied, and that overrunning it is
// reported rather than swallowed.
func TestRunReturnsShutdownErrorWhenGracePeriodExpires(t *testing.T) {
	s, _, baseURL := listenTestServer(t)
	s.config.ShutdownTimeout = 50 * time.Millisecond

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	s.Echo().GET("/stuck", func(c echo.Context) error {
		close(started)
		<-release
		return c.NoContent(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.run(ctx, noopStop) }()

	waitForServing(t, baseURL)
	go func() {
		resp, err := testClient().Get(baseURL + "/stuck")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	<-started
	cancel()

	err := awaitRun(t, errCh)
	if err == nil {
		t.Fatal("run() error = nil, want a shutdown timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("run() error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// TestRunReportsServeFailure covers the other half of C1: a genuine listen
// failure must still be surfaced, not swallowed alongside ErrServerClosed.
func TestRunReportsServeFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v, want nil", err)
	}
	defer occupied.Close()

	tests := []struct {
		name string
		addr string
	}{
		{name: "port already in use", addr: occupied.Addr().String()},
		{name: "malformed address", addr: "127.0.0.1:not-a-port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			s.Echo().HidePort = true
			s.config.BindAddress = tt.addr

			errCh := make(chan error, 1)
			go func() { errCh <- s.run(context.Background(), noopStop) }()

			err := awaitRun(t, errCh)
			if err == nil {
				t.Fatalf("run() error = nil, want a listen failure for %q", tt.addr)
			}
			if !strings.Contains(err.Error(), tt.addr) {
				t.Errorf("run() error = %v, want it to name the address %q", err, tt.addr)
			}
		})
	}
}

// TestRunReturnsServeFailureInsteadOfExiting checks the exported entry point:
// a library must hand a fatal-looking condition back to its caller and return,
// never call os.Exit on the caller's behalf. The error is what makes the
// failure actionable — without it a main() ending in Run() would exit 0 on a
// server that never bound a port.
func TestRunReturnsServeFailureInsteadOfExiting(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v, want nil", err)
	}
	defer occupied.Close()

	s, log := newTestServer(t)
	s.Echo().HidePort = true
	s.config.BindAddress = occupied.Addr().String()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Run() }()

	var runErr error
	select {
	case runErr = <-errCh:
	case <-time.After(runTimeout):
		t.Fatalf("Run() did not return within %s after a listen failure", runTimeout)
	}

	if runErr == nil {
		t.Fatalf("Run() error = nil, want a listen failure for %q", s.config.BindAddress)
	}
	if !strings.Contains(runErr.Error(), s.config.BindAddress) {
		t.Errorf("Run() error = %v, want it to name the address %q", runErr, s.config.BindAddress)
	}

	// The returned error is the single report. Logging it here as well would
	// describe one failure twice for any caller that handles what it is given.
	if errs := log.entriesAt(levelError); len(errs) != 0 {
		t.Errorf("logged %d error entries, want 0 now that the error is returned: %+v", len(errs), errs)
	}
}

func TestShutdownTimeoutFallsBackToDefault(t *testing.T) {
	tests := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "unset falls back", configured: 0, want: defaultShutdownTimeout},
		{name: "negative falls back", configured: -time.Second, want: defaultShutdownTimeout},
		{name: "configured value is used", configured: 3 * time.Second, want: 3 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			s.config.ShutdownTimeout = tt.configured

			if got := s.shutdownTimeout(); got != tt.want {
				t.Errorf("shutdownTimeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNewHttpConfigShutdownTimeout(t *testing.T) {
	t.Run("defaults to ten seconds", func(t *testing.T) {
		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.ShutdownTimeout != defaultShutdownTimeout {
			t.Errorf("ShutdownTimeout = %s, want %s", cfg.ShutdownTimeout, defaultShutdownTimeout)
		}
	})

	t.Run("reads HTTP_SHUTDOWN_TIMEOUT", func(t *testing.T) {
		t.Setenv("HTTP_SHUTDOWN_TIMEOUT", "45s")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if want := 45 * time.Second; cfg.ShutdownTimeout != want {
			t.Errorf("ShutdownTimeout = %s, want %s", cfg.ShutdownTimeout, want)
		}
	})
}
