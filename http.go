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
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Format: "method=${method}, status=${status},  path=${path}, remote=${remote_host}, latency=${latency_human}\n",
	}))

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
	e.GET("/healthz", ok)
	e.GET("/v1/healthz", ok)

	s.server = &http2.Server{
		MaxConcurrentStreams: 200,
		MaxReadFrameSize:     1024000,
		IdleTimeout:          10 * time.Second,
	}

	return s, nil
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
