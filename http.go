package server

import (
	"context"
	"net/http"
	"os"
	"os/signal"
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

	// Instantiate a new echo
	e := echo.New()

	s := &httpServer{
		config: cfg,
		echo:   e,
		logger: log,
	}

	// Set some useful middlewares
	e.Pre(middleware.RemoveTrailingSlash())
	e.Pre(middleware.RequestID())
	e.Use(middleware.BodyLimit(cfg.MaxBodyLimit))
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
	e.Debug = true

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

func (s httpServer) Run() {
	go func() {
		s.logger.Fatal(s.echo.StartH2CServer(s.config.BindAddress, s.server))
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.echo.Shutdown(ctx); err != nil {
		s.logger.Fatal(err)
	}

}

func ok(ctx echo.Context) error {
	return ctx.String(http.StatusOK, http.StatusText(http.StatusOK))
}
