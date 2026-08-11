package server

import (
	"context"
	"errors"
	"fmt"
	"net"
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

	// The baseline security header values. They are set on every response
	// unless HTTP_DISABLE_SECURITY_HEADERS says otherwise.
	//
	// nosniff stops a browser second-guessing a Content-Type, which is how a
	// JSON or upload endpoint ends up executing as script.
	//
	// SAMEORIGIN rather than DENY: this package serves a static route from the
	// same origin, and DENY would break an embed the same deployment owns while
	// adding nothing against the cross-origin framing SAMEORIGIN already
	// refuses.
	//
	// strict-origin-when-cross-origin keeps the full URL for same-origin
	// navigation, sends only the origin to another site, and sends nothing at
	// all on a downgrade. The paths this package carries name resources —
	// /v1/videos/42 — and a Referer is how those ids reach an analytics script
	// somebody else operates.
	//
	// X-XSS-Protection is set to 0, which is not a typo and not the echo
	// default. The legacy XSS auditor it enables was itself exploitable and has
	// been removed from every current browser; 0 is the value the OWASP secure
	// headers project now recommends, and it is sent rather than omitted so that
	// a browser still carrying the auditor does not turn it on by itself.
	secureContentTypeOptions = "nosniff"
	secureFrameOptions       = "SAMEORIGIN"
	secureReferrerPolicy     = "strict-origin-when-cross-origin"
	secureXSSProtection      = "0"

	// gzipMinLength is the smallest response worth compressing. Below roughly a
	// kilobyte the gzip framing costs more than the compression saves, and both
	// ends pay CPU for a response that came out larger.
	gzipMinLength = 1024
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
// are unaffected by it being named here. It is required: a nil one is refused
// here rather than substituted for.
func NewHTTP(cfg *HttpConfig, log Logger) (*httpServer, error) {
	// The logger first, before the configuration is even read. It is the one
	// argument this constructor cannot do without, and it is checked here for
	// the same reason the settings below are: a server that is going to fail
	// should fail in the call that built it, while the stack still names the
	// line that made the mistake.
	//
	// Refused rather than defaulted to a no-op, and that is the decision worth
	// stating. This package writes nothing on the caller's behalf and installs
	// no logger of its own; a silent no-op would leave a service serving
	// traffic with no access log, and — the part that actually hurts — no
	// record of a failing readiness check, whose cause is deliberately withheld
	// from the probe body and exists nowhere else. That service looks healthy
	// while being unobservable, and nothing anywhere says why. A returned error
	// is the same answer this constructor gives an unparseable body limit or an
	// origin that can never match: the mistake is reported once, at the point it
	// was made.
	//
	// This catches a nil interface, which is what `NewHTTP(cfg, nil)` and an
	// unassigned Logger variable both produce. It cannot catch a non-nil
	// interface holding a nil pointer — a nil *slog.Logger wrapped in an
	// adapter — because that is indistinguishable here from an implementation
	// whose methods handle a nil receiver, and rejecting it would break the
	// callers for whom it works.
	if log == nil {
		return nil, errors.New("logger is nil: this package logs every request and every failing readiness check through the logger it is given and installs no no-op in its place, so a nil one panics on the first request that is logged: pass a server.Logger, such as a three-method adapter over log/slog")
	}

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

	logExempt, err := cfg.logExemptPaths()
	if err != nil {
		return nil, err
	}

	allowedOrigins, err := cfg.allowedOrigins()
	if err != nil {
		return nil, err
	}

	corsMaxAge, err := cfg.corsMaxAge()
	if err != nil {
		return nil, err
	}

	hstsMaxAge, err := cfg.hstsMaxAge()
	if err != nil {
		return nil, err
	}

	rateLimit, err := cfg.rateLimit()
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

	// Set some useful middlewares. The order below is the whole design of the
	// chain: the first Use is the outermost wrapper and the last one sits
	// closest to the handler, so what is registered early sees every response
	// the ones after it produce, including their rejections.
	e.Pre(middleware.RemoveTrailingSlash())
	e.Pre(middleware.RequestID())

	// Baseline response headers, outermost of all, because a rejection is a
	// response too: a 413 from the body limit, a 429 from the rate limiter and
	// the 500 a recovered panic produces are all rendered by a browser and all
	// want the same headers as a 200. Nothing here reads the body or the route,
	// so being first costs four header writes.
	if !cfg.DisableSecurityHeaders {
		e.Use(securityHeaders(hstsMaxAge, cfg.HstsIncludeSubdomains))
	}

	// CORS, and its position is deliberate: outside every middleware that can
	// refuse a request. A cross-origin response without Access-Control-Allow-Origin
	// is not an error a browser can report — the status, the body and the reason
	// are all withheld from the script that asked, which sees only "CORS
	// error". Registered below the body limit, as it was, a browser upload over
	// the 10M limit would get exactly that: the real 413 arrives with no CORS
	// headers on it and the caller is left debugging the wrong problem. Above
	// it, the browser is told it was too large.
	//
	// The cost is that a preflight, which this middleware answers itself, no
	// longer reaches the access log below. That is the same trade the probe
	// paths make, and preflights are now cached for HTTP_CORS_MAX_AGE anyway.
	e.Use(corsMiddleware(allowedOrigins, cfg.allowedHeaders(), corsMaxAge))

	e.Use(middleware.BodyLimit(bodyLimit))

	// The access log, and then the recoverer inside it. That order is the whole
	// of finding M2: echo's request logger calls next(c) without deferring the
	// call that writes the entry, so a panic unwinding through it skips the log
	// entirely. With Recover registered outside — as it was — the panic passed
	// the logger on its way up and the only record of a 500 was the stack echo's
	// recoverer prints to stderr, unstructured and nowhere near the injected
	// logger. Inside, the panic is converted to a response before it reaches the
	// logger, which sees an ordinary 500 and records it. See requestLogger for
	// exactly what that entry contains.
	e.Use(requestLogger(log, logExempt))
	e.Use(middleware.Recover())

	// Rate limiting, when a deployment has asked for it. Inside the access log
	// on purpose: a 429 nobody can see in the log is indistinguishable from a
	// client that stopped calling, and the whole point of a limit is knowing
	// when it bites. Outside compression and the request timeout, because a
	// refused request should not pay for either.
	if rateLimit.enabled() {
		e.Use(rateLimiterMiddleware(rateLimit))
	}

	// Compression, when a deployment has asked for it. Inside the rate limiter
	// so a 429 is not compressed, and outside the request timeout so the
	// handler's writes pass through it.
	if cfg.Gzip {
		e.Use(gzipMiddleware(timeoutExempt))
	}

	// Bound handler execution, when a deployment has asked for a bound. Last
	// in the chain, so it wraps the handler and nothing else: the access log
	// above it records the 503 the client was actually sent, and a CORS
	// preflight is answered without a deadline it has no use for.
	if requestTimeout > 0 {
		e.Use(requestTimeoutMiddleware(requestTimeout, timeoutExempt))
	}

	// Static content. A route rather than middleware, so where it sits in this
	// function says nothing about the chain above: echo applies e.Use
	// middleware to every request when it serves, not when a route is added.
	e.Static("/static", "static")

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
//
// It holds the four routes this package registers itself and nothing else. A
// consumer whose own probes are at /health or /v1/health names them in
// HTTP_LOG_EXEMPT_PATHS, which the access log adds to this set; guessing at
// them here would have this package stop logging routes it does not own.
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

// onProbePath reports whether this request is for one of the liveness or
// readiness routes. It is the skipper behind three separate decisions — probe
// traffic is not logged, not rate limited and not compressed — so the set of
// paths is defined once.
func onProbePath(c echo.Context) bool {
	_, matched := probePaths[routePath(c)]
	return matched
}

// skipProbesAndExempt builds a skipper for the probe paths plus the configured
// route patterns in exempt. Two middlewares ask exactly this question of two
// different lists — the access log of HTTP_LOG_EXEMPT_PATHS, the compressor of
// HTTP_TIMEOUT_EXEMPT_PATHS — and writing the lookup once is what keeps their
// matching identical: exact, on the pattern the router settled on, so an
// exemption for "/v1/health" never covers "/v1/healthcheck".
func skipProbesAndExempt(exempt map[string]struct{}) middleware.Skipper {
	return func(c echo.Context) bool {
		if onProbePath(c) {
			return true
		}
		_, skipped := exempt[routePath(c)]
		return skipped
	}
}

// corsMiddleware builds the CORS middleware from a resolved allowlist.
//
// An empty origins list means no cross-origin access, and it has to be said
// explicitly: echo answers an empty AllowOrigins by substituting []string{"*"},
// so passing the empty list straight through would turn the closed default back
// into the wildcard it replaced. AllowOriginFunc takes precedence over
// AllowOrigins in that middleware, which is what makes the denial airtight
// whatever echo fills in behind it.
//
// AllowCredentials is not set, and there is no setting for it. Without it a
// cross-origin script can send a request but cannot attach the caller's cookies
// or read a response that depends on them, which is what keeps even
// HTTP_ALLLOWED_ORIGINS=* short of handing an authenticated session to any site
// that asks. Enabling credentials is a decision that belongs with whoever owns
// the authentication scheme, and it is one wrong pairing — credentials with a
// wildcard — away from being the CORS misconfiguration everybody writes about.
func corsMiddleware(origins, headers []string, maxAge int) echo.MiddlewareFunc {
	config := middleware.CORSConfig{
		AllowOrigins: origins,
		AllowHeaders: headers,
		AllowMethods: []string{
			http.MethodGet,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
		},
		MaxAge: maxAge,
	}

	if len(origins) == 0 {
		config.AllowOriginFunc = func(string) (bool, error) { return false, nil }
	}

	return middleware.CORSWithConfig(config)
}

// securityHeaders builds the baseline header middleware. hstsMaxAge is in
// seconds and is zero when no Strict-Transport-Security header should be sent.
//
// Content-Security-Policy is deliberately absent. A useful policy names the
// origins of a particular application's scripts, styles and frames, so this
// package could only guess; and a CSP that guesses wrong does not fail loudly,
// it silently stops a page loading half of itself. A consumer serving HTML adds
// its own with s.Echo().Use.
//
// The preload flag is absent for a stronger reason: it is a submission to a list
// compiled into browsers, it takes months to leave, and no library should be
// able to put a consumer's domain on it.
func securityHeaders(hstsMaxAge int, includeSubdomains bool) echo.MiddlewareFunc {
	return middleware.SecureWithConfig(middleware.SecureConfig{
		ContentTypeNosniff: secureContentTypeOptions,
		XFrameOptions:      secureFrameOptions,
		ReferrerPolicy:     secureReferrerPolicy,
		XSSProtection:      secureXSSProtection,
		HSTSMaxAge:         hstsMaxAge,
		// Echo's flag is the negative one, so this is inverted rather than
		// forgotten: subdomains are included only when a deployment asked.
		HSTSExcludeSubdomains: !includeSubdomains,
		HSTSPreloadEnabled:    false,
	})
}

// rateLimiterMiddleware builds the per-client-address rate limiter.
//
// The store is echo's in-memory one, with everything that implies and is
// documented on HttpConfig.RateLimit: the limit is per process, and the map
// holds a bucket per distinct client address until it has been idle for
// defaultRateLimitExpiry. The identifier is echo's default, c.RealIP(), which is
// why HTTP_REAL_IP_SOURCE has to be right before this is worth switching on.
//
// Probe paths are exempt. Rate limiting a readiness endpoint withdraws the
// instance from load balancing and rate limiting a liveness endpoint invites an
// orchestrator to restart it, so a burst of client traffic would take the
// process down by way of its own health checks.
func rateLimiterMiddleware(limit rateLimitSettings) echo.MiddlewareFunc {
	storeConfig := middleware.RateLimiterMemoryStoreConfig{
		Burst:     limit.burst,
		ExpiresIn: defaultRateLimitExpiry,
	}
	setRateLimit(&storeConfig.Rate, limit.perSecond)

	return middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Skipper: onProbePath,
		Store:   middleware.NewRateLimiterMemoryStoreWithConfig(storeConfig),
	})
}

// setRateLimit assigns a requests-per-second value to the store's rate field
// without naming that field's type.
//
// The field is a golang.org/x/time/rate.Limit. Converting to it by name would
// import golang.org/x/time here and promote it from an indirect dependency of
// echo to a direct one of this module, which changes go.mod — a change CI
// rejects, and one this package gets nothing for. The type parameter is
// inferred from the destination, so the conversion happens with no import at
// all.
func setRateLimit[T ~float64](dst *T, perSecond float64) {
	*dst = T(perSecond)
}

// gzipMiddleware builds the response compressor, skipping the routes where
// buffering costs more than compression saves.
//
// exempt is the timeout exemption list, reused rather than duplicated. A
// deployment names its streaming, download and upload routes there because they
// run long, and those are the same routes that must not be buffered and the
// ones most likely to be carrying media that is already compressed. A second
// variable listing the same paths would only be a way to get them out of step.
//
// Probe paths are skipped too: their bodies are two words, and compressing
// those makes them bigger while every load balancer in the deployment pays to
// decompress them.
func gzipMiddleware(exempt map[string]struct{}) echo.MiddlewareFunc {
	return middleware.GzipWithConfig(middleware.GzipConfig{
		MinLength: gzipMinLength,
		Skipper:   skipProbesAndExempt(exempt),
	})
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
//
// exempt is the configured HTTP_LOG_EXEMPT_PATHS set, skipped alongside the
// probe paths this package registers. It is how a consumer keeps its own
// high-volume probes — /health, /v1/health, /status — out of the log: those are
// routes this package never sees, so the built-in list cannot name them. The
// exemption stops at the log. A route named here is still rate limited and
// still compressed, because a variable named for logging must not be the way a
// route quietly loses its rate limit.
//
// It is registered outside middleware.Recover, and what a recovered panic looks
// like here follows from that. Recover handles the error itself — it calls
// c.Error and returns nil — so by the time this middleware reads its values the
// panic has already become an ordinary 500 response and next(c) has returned
// nil. The entry is therefore at error level by the status half of the rule
// below, v.Status >= 500, and carries no "error" field: there is no error left
// to report. The panic value and its stack go to stderr through echo's own
// recoverer and do not reach this logger at all, so an entry with a 500 and no
// error is the signal to go looking for them.
func requestLogger(log Logger, exempt map[string]struct{}) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		Skipper: skipProbesAndExempt(exempt),
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

// Addr reports the address the listener is bound to, and whether there is one.
//
// It exists for the case where the configured bind address does not name the
// port: HTTP_BIND_ADDRESS=":0" asks the kernel to choose, and until now the
// only way to learn what it chose was to reach through Echo() into echo's
// listener. That is a test seam and a service-registration seam both — a
// process that has to announce where it can be reached needs the resolved
// address, not the one it asked for.
//
// The second result is false until the server is listening, which is to say
// until Run has bound the port. It is a second result rather than a nil
// net.Addr on its own because that nil is the value that panics on .String()
// at the call site, one line later, with nothing in the signature to suggest
// it was possible. Written as
//
//	addr, ok := s.Addr()
//
// the absent case is at least named, and a caller that discards it has said so.
//
// Addr is safe to call from another goroutine while Run is starting up: it
// reads through echo's own accessor, which takes the lock that startup writes
// the listener under. Polling it is how a test learns the port without racing
// the bind.
//
// It goes on reporting the address after a drain has finished. Shutdown closes
// the listener without clearing it, so this answers "what did this server
// bind", not "is this server still serving" — liveness is at healthzPath.
func (s httpServer) Addr() (net.Addr, bool) {
	addr := s.echo.ListenerAddr()
	if addr == nil {
		return nil, false
	}
	return addr, true
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
