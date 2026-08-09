package server

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// waitForAddr polls Addr until the server is listening and returns the address
// it bound. It is how a test learns a kernel-chosen port without racing the
// bind, and it is the reason Addr exists.
func waitForAddr(t *testing.T, s *httpServer) string {
	t.Helper()

	deadline := time.Now().Add(runTimeout)
	for time.Now().Before(deadline) {
		if addr, ok := s.Addr(); ok {
			return addr.String()
		}
		time.Sleep(pollInterval)
	}

	t.Fatalf("Addr() reported no listener within %s", runTimeout)
	return ""
}

// TestAddrReportsNoListenerBeforeServing pins the lifecycle half of the
// accessor. There is no listener until Run binds one, and the answer to "what
// address is this on" before then is that there is not one yet — reported as a
// false second result rather than as a nil net.Addr on its own, which is a
// value that panics one line later at the caller.
func TestAddrReportsNoListenerBeforeServing(t *testing.T) {
	s, _ := newTestServer(t)

	addr, ok := s.Addr()
	if ok {
		t.Errorf("Addr() ok = true before Run, want false")
	}
	if addr != nil {
		t.Errorf("Addr() = %v before Run, want nil", addr)
	}
}

// TestAddrReportsAPreCreatedListener covers the path the test helpers take:
// listenTestServerWith binds a port itself and assigns echo.Listener, so the
// listener exists without Run ever having been called. Addr has to see that
// one too, or it would be an accessor that works everywhere except where this
// suite starts servers.
func TestAddrReportsAPreCreatedListener(t *testing.T) {
	s, _, baseURL := listenTestServer(t)

	addr, ok := s.Addr()
	if !ok {
		t.Fatal("Addr() ok = false with a listener already assigned, want true")
	}
	if want := "http://" + addr.String(); want != baseURL {
		t.Errorf("Addr() = %s, want the listener behind %s", addr, baseURL)
	}
}

// TestAddrReportsThePortChosenByTheKernel is the case the accessor was added
// for. HTTP_BIND_ADDRESS=":0" — or "127.0.0.1:0" here, so the test binds
// nothing routable — asks the kernel to choose the port, and the configured
// address never learns which one it got. Before this method the only way to
// find out was to reach through Echo() into echo's listener.
//
// The address is asserted to be usable, not merely non-empty: the server
// answers a liveness probe on it. And it is still reported after the drain,
// which is the documented behaviour and not an accident — Shutdown closes the
// listener without clearing it, so this reports what the server bound rather
// than whether it is still serving.
func TestAddrReportsThePortChosenByTheKernel(t *testing.T) {
	cfg := testConfig()
	cfg.BindAddress = "127.0.0.1:0"

	s, _ := newTestServerWith(t, cfg)
	s.Echo().HidePort = true
	s.config.ShutdownTimeout = runTimeout

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.run(ctx, noopStop) }()

	addr := waitForAddr(t, s)
	if addr == cfg.BindAddress {
		t.Fatalf("Addr() = %s, want the resolved address rather than the configured one", addr)
	}

	baseURL := "http://" + addr
	waitForServing(t, baseURL)

	resp, err := testClient().Get(baseURL + healthzPath)
	if err != nil {
		t.Fatalf("GET %s error = %v, want nil: the reported address must be the one serving", baseURL+healthzPath, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s status = %d, want %d", baseURL+healthzPath, resp.StatusCode, http.StatusOK)
	}

	cancel()
	if err := awaitRun(t, errCh); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}

	after, ok := s.Addr()
	if !ok {
		t.Fatal("Addr() ok = false after a drain, want true: it reports what was bound, not what is serving")
	}
	if after.String() != addr {
		t.Errorf("Addr() = %s after a drain, want the address it bound, %s", after, addr)
	}
}
