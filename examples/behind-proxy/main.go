// Command behind-proxy is a production-shaped configuration for running behind
// a TLS-terminating load balancer, and a route that lets you see the one
// setting people get wrong: where c.RealIP() reads the client address from.
//
// With HTTP_REAL_IP_SOURCE unset (the default, "peer") a client can put
// anything it likes in X-Forwarded-For and the server ignores it. With "xff"
// and a trusted proxy list, the header is believed — but only from a hop in
// that list. Naming proxies makes the list exhaustive: loopback and the private
// ranges stop being trusted implicitly.
//
// Run it in the default, header-ignoring mode:
//
//	HTTP_BIND_ADDRESS=:8080 go run ./examples/behind-proxy
//
// Or in the mode a deployment behind an in-cluster load balancer would use:
//
//	HTTP_BIND_ADDRESS=:8080 \
//	HTTP_REAL_IP_SOURCE=xff \
//	HTTP_TRUSTED_PROXIES=127.0.0.0/8,10.4.0.0/16 \
//	HTTP_ALLLOWED_ORIGINS=https://zion.example,https://admin.zion.example \
//	HTTP_MAX_BODY_LIMIT=2M \
//	HTTP_REQUEST_TIMEOUT=10s \
//	go run ./examples/behind-proxy
//
// See the README next to this file for the two curl invocations that show the
// difference.
package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/labstack/echo/v4"
	"github.com/zionmedianetwork/logam"
	"github.com/zionmedianetwork/server"
)

// clientView is what the echo route reports back about the caller: the address
// the server settled on, and the raw inputs that address was derived from.
type clientView struct {
	// ClientIP is c.RealIP(): the address the access log records, and the one
	// any allowlist, audit trail or rate limiter should be built on.
	ClientIP string `json:"client_ip"`
	// Peer is the address that actually opened the TCP connection. Behind a
	// load balancer this is the load balancer.
	Peer string `json:"peer"`
	// ForwardedFor and RealIPHeader are client-supplied data. They are here to
	// be compared against ClientIP, not to be used.
	ForwardedFor string `json:"x_forwarded_for"`
	RealIPHeader string `json:"x_real_ip"`
	Proto        string `json:"proto"`
}

func main() {
	// logam again, as a deployment of these services would use it: its Logger
	// satisfies server.Logger's three methods structurally, so it is passed
	// straight to NewHTTP. ../minimal shows the same thing with log/slog and no
	// logging dependency at all.
	log := logam.NewLogger(logam.Config{
		LogLevel:    "info",
		LogFormat:   "console",
		Environment: logam.EnvDevelopment,
	})

	if err := serve(log); err != nil {
		log.Errorw("http server stopped", "error", err)
		os.Exit(1)
	}
}

func serve(log logam.Logger) error {
	cfg, err := server.NewHttpConfig()
	if err != nil {
		return fmt.Errorf("read http configuration: %w", err)
	}

	// Reading the resolved config back is worth doing in a real service too:
	// these are the settings whose defaults are right for a laptop and wrong
	// for anything in front of the internet, and a warning at startup is
	// cheaper than finding out from an audit.
	warnAboutDevelopmentDefaults(cfg, log)

	s, err := server.NewHTTP(cfg, log)
	if err != nil {
		// Among the things refused here: HTTP_TRUSTED_PROXIES set while the
		// source is still "peer". The list would never be consulted, and a
		// silent no-op leaves an operator believing a proxy is trusted.
		return fmt.Errorf("build http server: %w", err)
	}

	e := s.Echo()
	e.GET("/v1/whoami", whoami)

	log.Infow("http server starting",
		"bind_address", cfg.BindAddress,
		"real_ip_source", cfg.RealIPSource,
		"trusted_proxies", cfg.TrustedProxies,
		"allowed_origins", cfg.AlllowedOrigins,
		"max_body_limit", cfg.MaxBodyLimit,
		"debug", cfg.Debug,
	)

	// h2c, in the clear. Everything this process serves is plaintext on the
	// wire, so the load balancer in front of it is not optional: it is where
	// TLS lives.
	return s.Run()
}

// warnAboutDevelopmentDefaults says out loud which settings are still at values
// that only make sense outside a deployment.
func warnAboutDevelopmentDefaults(cfg *server.HttpConfig, log logam.Logger) {
	// Note the three L's. The struct field is AlllowedOrigins, so the variable
	// is HTTP_ALLLOWED_ORIGINS; there is no spelling of this with two.
	if slices.Contains(cfg.AlllowedOrigins, "*") {
		log.Warnw("cors allows every origin",
			"variable", "HTTP_ALLLOWED_ORIGINS",
			"fix", "name the origins your browser clients are actually served from",
		)
	}

	if cfg.RealIPSource == server.RealIPSourcePeer {
		log.Warnw("client address is the peer address and forwarding headers are ignored",
			"variable", "HTTP_REAL_IP_SOURCE",
			"effect", "behind a load balancer every request is logged as the load balancer",
			"fix", "set it to "+server.RealIPSourceXFF+" together with HTTP_TRUSTED_PROXIES",
		)
	}

	if cfg.RealIPSource == server.RealIPSourceXFF && len(cfg.TrustedProxies) == 0 {
		log.Warnw("x-forwarded-for is believed from any loopback or private address",
			"variable", "HTTP_TRUSTED_PROXIES",
			"effect", "anything inside the cluster network can name its own client address",
		)
	}

	if cfg.Debug {
		log.Warnw("debug is on: handler errors are copied into response bodies",
			"variable", "HTTP_DEBUG",
			"effect", "driver errors, file paths and connection-string credentials reach callers",
		)
	}
}

// whoami reports the address the server settled on next to the raw inputs, so
// the effect of HTTP_REAL_IP_SOURCE is visible in one response.
func whoami(c echo.Context) error {
	req := c.Request()

	return server.HTTPResponse(c, clientView{
		ClientIP:     c.RealIP(),
		Peer:         req.RemoteAddr,
		ForwardedFor: req.Header.Get(echo.HeaderXForwardedFor),
		RealIPHeader: req.Header.Get(echo.HeaderXRealIP),
		Proto:        req.Proto,
	})
}
