package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/zionmedianetwork/logam"
	"golang.org/x/net/http2"
)

const (
	healthzPath   = "/healthz"
	v1HealthzPath = "/v1/healthz"

	// requestLogMessage is the message every access log entry carries; the
	// detail lives in the structured fields.
	requestLogMessage = "request"
)

type httpServer struct {
	config *HttpConfig
	echo   *echo.Echo
	server *http2.Server
	logger logam.Logger
}

func NewHTTP(cfg *HttpConfig, log logam.Logger) (*httpServer, error) {
	var err error
	if cfg == nil {
		cfg, err = NewHttpConfig()
		if err != nil {
			return nil, err
		}
	}

	// Resolve everything the configuration decides before any of it is wired
	// up, so a misconfigured server is reported here and never half-built.
	bodyLimit, err := cfg.bodyLimit()
	if err != nil {
		return nil, err
	}

	ipExtractor, err := cfg.ipExtractor()
	if err != nil {
		return nil, err
	}

	// Instantiate a new echo
	e := echo.New()

	s := &httpServer{
		config: cfg,
		echo:   e,
		logger: log,
	}

	// Where the client address comes from. Left unset, echo falls back to
	// legacy behaviour and takes X-Forwarded-For from whoever sent it, so
	// c.RealIP() — and with it every access log line, and any allowlist or
	// rate limit built on top — would report whatever the caller asked for.
	e.IPExtractor = ipExtractor

	// Set some useful middlewares
	e.Pre(middleware.RemoveTrailingSlash())
	e.Pre(middleware.RequestID())
	e.Use(middleware.BodyLimit(bodyLimit))
	e.Use(middleware.Recover())
	e.Use(requestLogger(log))

	// Static content
	e.Static("/static", "static")

	// CORS
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: cfg.AlllowedOrigins,
		AllowMethods: []string{
			http.MethodGet,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
		},
	}))
	// Hide default echo banner
	e.HideBanner = true
	// Debug decides whether echo's default error handler copies the error a
	// handler returned into the response body. Off unless a deployment asks
	// for it, so a driver or filesystem error is not narrated to the caller.
	e.Debug = cfg.Debug

	// Read/Write timeout
	e.Server.ReadTimeout = cfg.ReadTimeout
	e.Server.WriteTimeout = cfg.WriteTimeout

	// Health check routes
	e.GET(healthzPath, ok)
	e.GET(v1HealthzPath, ok)

	s.server = &http2.Server{
		MaxConcurrentStreams: 200,
		MaxReadFrameSize:     1024000,
		IdleTimeout:          10 * time.Second,
	}

	return s, nil
}

// healthCheckPaths holds the route paths that are excluded from request
// logging. Probe traffic is high volume and carries no information, so logging
// it only drowns out real requests.
var healthCheckPaths = map[string]struct{}{
	healthzPath:   {},
	v1HealthzPath: {},
}

// skipRequestLog reports whether the request logger should ignore this request.
// It matches on the routed path so that query strings never affect the decision.
func skipRequestLog(c echo.Context) bool {
	path := c.Path()
	if path == "" {
		path = c.Request().URL.Path
	}
	_, skipped := healthCheckPaths[path]
	return skipped
}

// requestLogger builds the access log middleware. It emits one structured entry
// per request through the injected logam logger, at error level when the handler
// chain failed or answered with a server error, and at info level otherwise.
func requestLogger(log logam.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		Skipper: skipRequestLog,
		// Let the global error handler run before the values are read, so the
		// logged status and response size are the ones the client actually got
		// rather than the still-untouched defaults.
		HandleError:     true,
		LogRequestID:    true,
		LogRemoteIP:     true,
		LogMethod:       true,
		LogURI:          true,
		LogURIPath:      true,
		LogStatus:       true,
		LogLatency:      true,
		LogError:        true,
		LogResponseSize: true,
		LogValuesFunc: func(_ echo.Context, v middleware.RequestLoggerValues) error {
			fields := []interface{}{
				"request_id", v.RequestID,
				"method", v.Method,
				"path", v.URIPath,
				"uri", v.URI,
				"status", v.Status,
				"remote_ip", v.RemoteIP,
				"latency", v.Latency.String(),
				"bytes_out", v.ResponseSize,
			}

			if v.Error != nil {
				fields = append(fields, "error", v.Error.Error())
			}

			if v.Error != nil || v.Status >= http.StatusInternalServerError {
				log.Errorw(requestLogMessage, fields...)
				return nil
			}

			log.Infow(requestLogMessage, fields...)
			return nil
		},
	})
}

func (s httpServer) Echo() *echo.Echo {
	return s.echo
}

// Run starts the server and blocks until the process is asked to terminate
// with SIGINT or SIGTERM, then drains in-flight requests before returning.
//
// Run returns nil when the drain completes within the configured grace
// period. It returns a non-nil error when the server never got as far as
// serving — a failed bind being the usual case — or when the drain overruns
// that grace period and in-flight requests were cut off.
//
// The returned error is the only report Run makes: nothing is logged on the
// caller's behalf, so the failure is described once, by whoever owns the
// process, with whatever severity and context that owner considers right.
//
// Run never terminates the calling process. Deciding that a failure to serve
// warrants a non-zero exit status belongs to the application, not to this
// package; a caller that discards the returned error is choosing to keep
// running, and gets no diagnostics for that choice.
func (s httpServer) Run() error {
	// Register the signal handlers before anything starts listening. A signal
	// that races with startup is then caught by this handler instead of
	// falling through to Go's default action, which is to kill the process.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return s.run(ctx, stop)
}

// run serves until ctx is cancelled and then drains. It is the testable core
// of Run: the caller owns the signal wiring, so a test can drive the whole
// shutdown path with a plain context.
//
// stop is called the moment the drain starts. That deregisters the signal
// handlers and restores the default disposition, so a second SIGINT or
// SIGTERM aborts a drain that is not making progress instead of being
// swallowed — the familiar "press Ctrl-C again to force quit" behaviour, and
// the same escalation path a runtime takes before it resorts to SIGKILL.
func (s httpServer) run(ctx context.Context, stop func()) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.serve()
	}()

	select {
	case err := <-serveErr:
		// The server stopped on its own before any shutdown was requested, so
		// there is nothing to drain. This is the genuine startup failure path.
		return err
	case <-ctx.Done():
	}

	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout())
	defer cancel()

	if err := s.echo.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown http server: %w", err)
	}

	// Shutdown has returned, so the serve goroutine is done or about to be.
	// Collecting its result keeps it from outliving Run.
	return <-serveErr
}

// serve runs the listener and normalises its result. http.ErrServerClosed is
// how the standard library reports "Shutdown was called" — a success signal,
// not a failure — so it must not be propagated.
func (s httpServer) serve() error {
	err := s.echo.StartH2CServer(s.config.BindAddress, s.server)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve http on %s: %w", s.config.BindAddress, err)
}

// shutdownTimeout is the grace period given to in-flight requests. A
// non-positive value means the config was built by hand without one, and
// using it as-is would expire the context immediately and sever every live
// request, so fall back to the package default.
func (s httpServer) shutdownTimeout() time.Duration {
	if s.config.ShutdownTimeout <= 0 {
		return defaultShutdownTimeout
	}
	return s.config.ShutdownTimeout
}

func ok(ctx echo.Context) error {
	return ctx.String(http.StatusOK, http.StatusText(http.StatusOK))
}
