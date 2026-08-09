//go:build unix

package server

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// runOnSignal starts Run, waits until it is provably serving, delivers sig to
// this process and asserts Run drains and returns nil. A clean shutdown is not
// a failure, so the nil return is half the contract: a caller that turns a
// non-nil Run error into a non-zero exit status must not be made to fail on
// an ordinary SIGTERM from its orchestrator.
//
// Sending a real signal to the test binary is only safe because Run installs
// its handlers before anything starts listening: a server that answers a
// health check therefore proves the handler is registered, so the signal
// cannot fall through to Go's default action and kill the test run. Every wait
// is bounded so a regression fails the test rather than hanging it.
func runOnSignal(t *testing.T, sig syscall.Signal) {
	t.Helper()

	s, log, baseURL := listenTestServer(t)

	errCh := make(chan error, 1)
	go func() { errCh <- s.Run() }()

	waitForServing(t, baseURL)

	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		t.Fatalf("syscall.Kill(%v) error = %v, want nil", sig, err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run() error = %v, want nil on a graceful shutdown after %v", err, sig)
		}
	case <-time.After(runTimeout):
		t.Fatalf("Run() did not return within %s after %v", runTimeout, sig)
	}

	if errs := log.entriesAt(levelError); len(errs) != 0 {
		t.Errorf("logged %d error entries on a signalled shutdown, want 0: %+v", len(errs), errs)
	}
}

// TestRunShutsDownOnSIGTERM covers C2. SIGTERM is what Docker, Kubernetes and
// systemd send; the old code registered os.Interrupt only, so SIGTERM took its
// default action and killed the process without ever draining.
func TestRunShutsDownOnSIGTERM(t *testing.T) {
	runOnSignal(t, syscall.SIGTERM)
}

// TestRunShutsDownOnSIGINT guards the behaviour that already worked, so the
// SIGTERM fix cannot regress Ctrl-C.
func TestRunShutsDownOnSIGINT(t *testing.T) {
	runOnSignal(t, syscall.SIGINT)
}
