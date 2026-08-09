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
	"golang.org/x/net/http2"
)

const (
	// The liveness paths. They answer a static 200 for as long as the process
	// is serving and say nothing about its dependencies: a liveness probe that
	// failed with the database would have Kubernetes restart a pod whose only
	// problem is somewhere else. Readiness is at readyzPath and v1ReadyzPath.
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
	logger Logger
	// readiness is held by pointer so that the value-receiver methods on this
	// type share one registry: a copy of httpServer must see the checks the
	// consumer registered on the original, and must not copy the lock.
	readiness *readinessRegistry
}

// NewHTTP builds a server from cfg, or from the environment when cfg is nil,
// and returns the first configuration error it finds rather than a half-built
// server.
//
// log is the three-method Logger this package writes its access log and its
// readiness log through. Any logger satisfies it structurally, including
// github.com/zionmedianetwork/logam's Logger, so callers passing one of those
// are unaffected by it being named here.
func NewHTTP(cfg *HttpConfig, log Logger) (*httpServer, error) {
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

	requestTimeout, err := cfg.requestTimeout()
	if err != nil {
		return nil, err
	}

	timeoutExempt, err := cfg.timeoutExemptPaths()
	if err != nil {
		return nil, err
	}

	// Instantiate a new echo
	e := echo.New()

	s := &httpServer{
		config:    cfg,
		echo:      e,
		logger:    log,
		readiness: &readinessRegistry{},
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
	// Bound handler execution, when a deployment has asked for a bound. Last
	// in the chain, so it wraps the handler and nothing else: the access log
	// above it records the 503 the client was actually sent, and a CORS
	// preflight is answered without a deadline it has no use for.
	if requestTimeout > 0 {
		e.Use(requestTimeoutMiddleware(requestTimeout, timeoutExempt))
	}

	// Hide default echo banner
	e.HideBanner = true
	// Debug decides whether echo's default error handler copies the error a
	// handler returned into the response body. Off unless a deployment asks
	// for it, so a driver or filesystem error is not narrated to the caller.
	e.Debug = cfg.Debug

	// Read/Write timeout
	e.Server.ReadTimeout = cfg.ReadTimeout
	e.Server.WriteTimeout = cfg.WriteTimeout

	// Liveness: is this process serving at all. Static, and deliberately so.
	e.GET(healthzPath, ok)
	e.GET(v1HealthzPath, ok)

	// Readiness: should this instance be sent traffic. Runs whatever checks
	// the consumer registered with RegisterReadinessCheck.
	e.GET(readyzPath, s.readyz)
	e.GET(v1ReadyzPath, s.readyz)

	s.server = &http2.Server{
		MaxConcurrentStreams: 200,
		MaxReadFrameSize:     1024000,
		IdleTimeout:          10 * time.Second,
	}

	return s, nil
}

// probePaths holds the route paths that are excluded from request logging:
// the liveness and readiness endpoints. Probe traffic is high volume and
// carries no information, so logging it only drowns out real requests — and
// readiness is polled harder than liveness, since a load balancer asks it
// about every instance.
var probePaths = map[string]struct{}{
	healthzPath:   {},
	v1HealthzPath: {},
	readyzPath:    {},
	v1ReadyzPath:  {},
}

// routePath returns the path a skipper should match on: the pattern the router
// settled on, so that query strings never affect the decision and a
// parameterised route is one path rather than one per value. It falls back to
// the request URL for a request that never matched a route.
func routePath(c echo.Context) string {
	if path := c.Path(); path != "" {
		return path
	}
	return c.Request().URL.Path
}

// skipRequestLog reports whether the request logger should ignore this request.
func skipRequestLog(c echo.Context) bool {
	_, skipped := probePaths[routePath(c)]
	return skipped
}

// requestTimeoutMiddleware bounds handler execution at timeout, exempting the
// routes named in exempt.
//
// It works by giving c.Request().Context() a deadline, and this is the load
// bearing caveat: cancellation is a message, not a stop. A handler that selects
// on its request context, or that passes it down to the database driver and the
// HTTP clients it calls, is unblocked at the deadline and the caller gets a
// clean 503. A handler that ignores the context runs to completion exactly as
// it does today, holding its goroutine and its connection until it finishes;
// all this middleware adds for that handler is the 503, and only if it happens
// to return the expired context's error. Propagating c.Request().Context() into
// every blocking call is what makes the timeout real.
//
// The deprecated middleware.Timeout is not used here on purpose: it runs the
// handler on another goroutine and answers on its behalf, which leaves the
// handler running anyway, races the response writer, and defeats the Recover
// middleware.
func requestTimeoutMiddleware(timeout time.Duration, exempt map[string]struct{}) echo.MiddlewareFunc {
	return middleware.ContextTimeoutWithConfig(middleware.ContextTimeoutConfig{
		Timeout: timeout,
		Skipper: func(c echo.Context) bool {
			_, skipped := exempt[routePath(c)]
			return skipped
		},
	})
}

// requestLogger builds the access log middleware. It emits one structured entry
// per request through the injected logger, at error level when the handler
// chain failed or answered with a server error, and at info level otherwise.
func requestLogger(log Logger) echo.MiddlewareFunc {
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

// ok is the liveness handler. It reports that this process is running and
// answering, and nothing else — a dependency it cannot reach is readiness's
// business, at readyzPath.
func ok(ctx echo.Context) error {
	return ctx.String(http.StatusOK, http.StatusText(http.StatusOK))
}
