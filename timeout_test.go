package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	// testRequestTimeout is the bound the timeout tests configure. Short
	// enough to keep them quick, long enough to survive a loaded machine, and
	// well under testConfig's one second write timeout so the middleware is
	// what expires rather than the transport.
	testRequestTimeout = 40 * time.Millisecond
	// testHandlerLifetime is how long a handler in these tests would run if
	// nothing cancelled it. Far beyond the timeout, so a handler that reaches
	// the end of it has provably not been cancelled.
	testHandlerLifetime = 5 * time.Second
	// serviceUnavailableBody is what echo's error handler writes for the 503
	// the timeout middleware returns.
	serviceUnavailableBody = `{"message":"Service Unavailable"}`
)

// timeoutConfig is testConfig with the request timeout switched on.
func timeoutConfig() *HttpConfig {
	cfg := testConfig()
	cfg.RequestTimeout = testRequestTimeout
	return cfg
}

// cancellableHandler returns a handler that waits on its request context and
// reports what it saw, along with the channel carrying that report. A nil error
// on the channel means the handler ran to completion untouched.
func cancellableHandler(t *testing.T, body string) (echo.HandlerFunc, <-chan error) {
	t.Helper()

	observed := make(chan error, 1)
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		select {
		case <-ctx.Done():
			observed <- ctx.Err()
			// Handing the context's error back is what a handler that
			// propagated the context does; the middleware turns it into the
			// 503. See requestTimeoutMiddleware on why that cooperation is
			// required.
			return ctx.Err()
		case <-time.After(testHandlerLifetime):
			observed <- nil
			return c.String(http.StatusOK, body)
		}
	}, observed
}

// awaitObservation waits for the handler's report of what its context did.
func awaitObservation(t *testing.T, observed <-chan error) error {
	t.Helper()

	select {
	case err := <-observed:
		return err
	case <-time.After(runTimeout):
		t.Fatalf("handler did not report within %s", runTimeout)
		return nil
	}
}

// TestRequestTimeoutCancelsHandlerAndAnswers503 covers H3. Nothing bounded
// handler execution and nothing propagated cancellation: a handler outlived the
// client that asked for it, holding its goroutine and its database connection
// until it finished, while the client was cut off by the write timeout with a
// transport error and no status at all.
func TestRequestTimeoutCancelsHandlerAndAnswers503(t *testing.T) {
	s, log := newTestServerWith(t, timeoutConfig())
	handler, observed := cancellableHandler(t, "too late")
	s.Echo().GET("/slow", handler)

	start := time.Now()
	rec := do(t, s, httptest.NewRequest(http.MethodGet, "/slow", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != serviceUnavailableBody {
		t.Errorf("body = %s, want %s", body, serviceUnavailableBody)
	}

	// The response is the timeout's, not the handler's: the request returned
	// long before the handler would have finished on its own.
	if elapsed >= testHandlerLifetime {
		t.Errorf("request took %s, want it cut off near %s", elapsed, testRequestTimeout)
	}

	// The handler side of the same claim. Without this the middleware could be
	// answering 503 while the handler carried on in the background, which is
	// the failure being fixed rather than the fix.
	err := awaitObservation(t, observed)
	if err == nil {
		t.Fatal("handler ran to completion, want it cancelled at the request timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("handler context error = %v, want context.DeadlineExceeded", err)
	}

	// The access log records what the client got, so a timeout is visible as a
	// 503 in the logs rather than only as a latency.
	wantField(t, onlyEntry(t, log), "status", http.StatusServiceUnavailable)
}

// TestRequestTimeoutAnswers503OverTheWire is the same case over a real
// connection, which is where the original defect was reproduced: with the
// handler outliving the write timeout the client saw `stream error;
// INTERNAL_ERROR` on h2c and an EOF on HTTP/1.1. The write timeout here is
// generous relative to the request timeout, which is the ordering NewHTTP now
// enforces, so the middleware wins the race and writes a status.
func TestRequestTimeoutAnswers503OverTheWire(t *testing.T) {
	cfg := timeoutConfig()
	cfg.WriteTimeout = 5 * time.Second

	s, _, baseURL := listenTestServerWith(t, cfg)
	handler, observed := cancellableHandler(t, "too late")
	s.Echo().GET("/slow", handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.run(ctx, noopStop) }()

	waitForServing(t, baseURL)

	resp, err := testClient().Get(baseURL + "/slow")
	if err != nil {
		t.Fatalf("client error = %v, want a 503 response rather than a transport failure", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body failed: %v, want a complete body", err)
	}

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if body := strings.TrimSpace(string(payload)); body != serviceUnavailableBody {
		t.Errorf("body = %s, want %s", body, serviceUnavailableBody)
	}

	if err := awaitObservation(t, observed); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("handler context error = %v, want context.DeadlineExceeded", err)
	}

	cancel()
	if err := awaitRun(t, errCh); err != nil {
		t.Errorf("run() error = %v, want nil", err)
	}
}

// TestRequestTimeoutExemptsConfiguredPaths covers the reason the exemption
// exists: a media network has routes that are meant to run long — a large
// download, an upload, an event stream — and a blanket timeout would cut every
// one of them off mid-body.
func TestRequestTimeoutExemptsConfiguredPaths(t *testing.T) {
	const streamPath = "/videos/:id/stream"

	cfg := timeoutConfig()
	cfg.TimeoutExemptPaths = []string{streamPath, " /uploads "}

	s, _ := newTestServerWith(t, cfg)

	// The exempt handler proves two things: no deadline was attached to its
	// request context at all, and it is still running well past the point the
	// timeout would have cancelled it.
	type observation struct {
		hasDeadline bool
		err         error
	}
	seen := make(chan observation, 1)
	s.Echo().GET(streamPath, func(c echo.Context) error {
		ctx := c.Request().Context()
		_, hasDeadline := ctx.Deadline()
		time.Sleep(4 * testRequestTimeout)
		seen <- observation{hasDeadline: hasDeadline, err: ctx.Err()}
		return c.String(http.StatusOK, "streamed")
	})

	rec := do(t, s, httptest.NewRequest(http.MethodGet, "/videos/42/stream", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "streamed" {
		t.Errorf("body = %q, want %q", got, "streamed")
	}

	got := <-seen
	if got.hasDeadline {
		t.Error("exempt handler was given a request deadline, want none")
	}
	if got.err != nil {
		t.Errorf("exempt handler context error = %v, want nil after %s", got.err, 4*testRequestTimeout)
	}
}

// TestRequestTimeoutAppliesToUnexemptedRoutes is the other half of the
// exemption: naming one route must not disarm the timeout everywhere else.
func TestRequestTimeoutAppliesToUnexemptedRoutes(t *testing.T) {
	cfg := timeoutConfig()
	cfg.TimeoutExemptPaths = []string{"/uploads"}

	s, _ := newTestServerWith(t, cfg)
	handler, observed := cancellableHandler(t, "too late")
	s.Echo().GET("/videos", handler)

	if rec := do(t, s, httptest.NewRequest(http.MethodGet, "/videos", nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if err := awaitObservation(t, observed); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("handler context error = %v, want context.DeadlineExceeded", err)
	}
}

// TestRequestTimeoutLeavesFastHandlersAlone guards the ordinary case: a handler
// that finishes inside the bound answers exactly as it did before.
func TestRequestTimeoutLeavesFastHandlersAlone(t *testing.T) {
	s, _ := newTestServerWith(t, timeoutConfig())
	s.Echo().GET("/fast", func(c echo.Context) error {
		return c.String(http.StatusOK, "done")
	})

	rec := do(t, s, httptest.NewRequest(http.MethodGet, "/fast", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "done" {
		t.Errorf("body = %q, want %q", got, "done")
	}
}

// TestRequestTimeoutIsOffByDefault pins the deliberate default. This package
// cannot see the routes it carries, so it does not guess a bound for them: an
// upgrade must not start answering 503 to a request that used to be served.
// The behaviour is opt-in through HTTP_REQUEST_TIMEOUT.
func TestRequestTimeoutIsOffByDefault(t *testing.T) {
	cfg, err := NewHttpConfig()
	if err != nil {
		t.Fatalf("NewHttpConfig() error = %v, want nil", err)
	}
	if cfg.RequestTimeout != defaultRequestTimeout {
		t.Fatalf("RequestTimeout = %s, want %s", cfg.RequestTimeout, defaultRequestTimeout)
	}
	if defaultRequestTimeout != 0 {
		t.Fatalf("defaultRequestTimeout = %s, want it to be zero, meaning no timeout", defaultRequestTimeout)
	}

	// A config nobody edited must leave the request context undeadlined, which
	// is what "off" has to mean for a handler.
	s, _ := newTestServerWith(t, testConfig())
	deadlines := make(chan bool, 1)
	s.Echo().GET("/probe", func(c echo.Context) error {
		_, hasDeadline := c.Request().Context().Deadline()
		deadlines <- hasDeadline
		return c.NoContent(http.StatusOK)
	})

	do(t, s, httptest.NewRequest(http.MethodGet, "/probe", nil))

	if <-deadlines {
		t.Error("request context carries a deadline with no request timeout configured")
	}
}

// TestNewHTTPRejectsUnusableRequestTimeout covers the interaction that decides
// whether this feature works at all. net/http closes the connection once
// WriteTimeout passes, so a request timeout at or above it cancels handlers and
// never manages to write the 503 that makes the cancellation visible — the
// client is left with the same transport error as before. That is refused at
// the choke point rather than shipped as a setting that looks configured.
func TestNewHTTPRejectsUnusableRequestTimeout(t *testing.T) {
	tests := []struct {
		name         string
		timeout      time.Duration
		writeTimeout time.Duration
		wantText     []string
	}{
		{
			name:         "equal to the write timeout",
			timeout:      15 * time.Second,
			writeTimeout: 15 * time.Second,
			wantText:     []string{"15s", "HTTP_WRITETIMEOUT"},
		},
		{
			name:         "above the write timeout",
			timeout:      30 * time.Second,
			writeTimeout: 15 * time.Second,
			wantText:     []string{"30s", "15s", "HTTP_REQUEST_TIMEOUT"},
		},
		{
			name:         "negative is not a way to disable it",
			timeout:      -time.Second,
			writeTimeout: 15 * time.Second,
			wantText:     []string{"-1s", "positive"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.RequestTimeout = tt.timeout
			cfg.WriteTimeout = tt.writeTimeout

			err := newHTTPError(t, cfg)
			for _, want := range tt.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("NewHTTP() error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

// TestNewHTTPRejectsUnusableRequestTimeoutFromEnv walks the same rejection in
// through the environment, which is how an operator will actually cause it: the
// default write timeout is 15s, so the obvious "give requests 30 seconds" is
// exactly the setting that cannot work.
func TestNewHTTPRejectsUnusableRequestTimeoutFromEnv(t *testing.T) {
	t.Setenv("HTTP_REQUEST_TIMEOUT", "30s")

	err := newHTTPError(t, nil)
	if !strings.Contains(err.Error(), "30s") {
		t.Errorf("NewHTTP() error = %v, want it to name the value %q", err, "30s")
	}
}

// TestNewHTTPAcceptsRequestTimeoutBelowWriteTimeout is the boundary from the
// other side: a bound that leaves the connection open long enough to answer is
// accepted.
func TestNewHTTPAcceptsRequestTimeoutBelowWriteTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.WriteTimeout = 15 * time.Second
	cfg.RequestTimeout = 14*time.Second + 999*time.Millisecond

	newTestServerWith(t, cfg)
}

// TestNewHTTPRejectsMalformedTimeoutExemptPath guards a silent failure: an
// entry without a leading slash can never match a routed path, so the route it
// names would be timed out anyway while the config says it is exempt.
func TestNewHTTPRejectsMalformedTimeoutExemptPath(t *testing.T) {
	tests := []struct {
		name    string
		paths   []string
		timeout time.Duration
	}{
		{name: "no leading slash", paths: []string{"videos/:id/stream"}, timeout: testRequestTimeout},
		{name: "second entry is malformed", paths: []string{"/uploads", "stream"}, timeout: testRequestTimeout},
		{
			// Checked even where it is inert, so the mistake surfaces when it
			// is made rather than when the timeout is later switched on.
			name:  "checked even with the timeout off",
			paths: []string{"stream"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.RequestTimeout = tt.timeout
			cfg.TimeoutExemptPaths = tt.paths

			err := newHTTPError(t, cfg)
			if !strings.Contains(err.Error(), "stream") {
				t.Errorf("NewHTTP() error = %v, want it to name the offending entry", err)
			}
		})
	}
}

// TestTimeoutExemptPathsIgnoresBlankEntries guards the same edge of envconfig
// that TrustedProxies has: the variable set to the empty string arrives as a
// one-element list holding it.
func TestTimeoutExemptPathsIgnoresBlankEntries(t *testing.T) {
	cfg := testConfig()
	cfg.TimeoutExemptPaths = []string{"", "   ", " /uploads "}

	exempt, err := cfg.timeoutExemptPaths()
	if err != nil {
		t.Fatalf("timeoutExemptPaths() error = %v, want nil", err)
	}
	if len(exempt) != 1 {
		t.Fatalf("timeoutExemptPaths() = %v, want just the one real entry", exempt)
	}
	if _, ok := exempt["/uploads"]; !ok {
		t.Errorf("timeoutExemptPaths() = %v, want it to hold %q trimmed", exempt, "/uploads")
	}
}

func TestNewHttpConfigRequestTimeoutSettings(t *testing.T) {
	t.Run("defaults to off with nothing exempt", func(t *testing.T) {
		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.RequestTimeout != 0 {
			t.Errorf("RequestTimeout = %s, want 0", cfg.RequestTimeout)
		}
		if len(cfg.TimeoutExemptPaths) != 0 {
			t.Errorf("TimeoutExemptPaths = %q, want empty by default", cfg.TimeoutExemptPaths)
		}
	})

	t.Run("reads HTTP_REQUEST_TIMEOUT and HTTP_TIMEOUT_EXEMPT_PATHS", func(t *testing.T) {
		t.Setenv("HTTP_REQUEST_TIMEOUT", "5s")
		t.Setenv("HTTP_TIMEOUT_EXEMPT_PATHS", "/videos/:id/stream,/uploads")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if want := 5 * time.Second; cfg.RequestTimeout != want {
			t.Errorf("RequestTimeout = %s, want %s", cfg.RequestTimeout, want)
		}
		if want := []string{"/videos/:id/stream", "/uploads"}; !slices.Equal(cfg.TimeoutExemptPaths, want) {
			t.Errorf("TimeoutExemptPaths = %q, want %q", cfg.TimeoutExemptPaths, want)
		}
	})
}
