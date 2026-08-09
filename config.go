package server

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/labstack/echo/v4"
	"github.com/labstack/gommon/bytes"
)

const (
	defaultReadTimeout     = time.Second * 15
	defaultWriteTimeout    = time.Second * 15
	defaultShutdownTimeout = time.Second * 10
	defaultStaticPath      = "/static"
	// defaultMaxBodyLimit is the largest request body accepted unless a
	// deployment raises it. The previous default, 51200M, was parsed as 51.2
	// GB of decimal megabytes: a size no process can buffer, so in practice
	// no limit at all.
	defaultMaxBodyLimit = "10M"
)

// The values accepted by HttpConfig.RealIPSource. They decide where
// echo.Context.RealIP() reads the client address from, which is a property of
// the deployment topology rather than of this package, and so has to be
// configurable.
const (
	// RealIPSourcePeer uses the address of the host that opened the
	// connection and ignores forwarding headers entirely. It is the default
	// because those headers are client-supplied data: with nothing trusted in
	// front, any caller can claim any address it likes.
	RealIPSourcePeer = "peer"
	// RealIPSourceXFF reads X-Forwarded-For, walking the chain from the right
	// and stopping at the first hop that is not a trusted proxy. Use it only
	// when a proxy this server trusts is always in front and that proxy
	// overwrites, rather than appends to, an incoming X-Forwarded-For.
	RealIPSourceXFF = "xff"
)

type HttpConfig struct {
	BindAddress  string `split_words:"true" default:":8080"`
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// ShutdownTimeout is the grace period in-flight requests are given to
	// finish once a termination signal arrives. Read from
	// HTTP_SHUTDOWN_TIMEOUT; defaults to defaultShutdownTimeout.
	ShutdownTimeout time.Duration `split_words:"true"`
	// MaxBodyLimit is the largest request body accepted, written in gommon's
	// byte notation: 10M, 512K, 4MiB. Read from HTTP_MAX_BODY_LIMIT; defaults
	// to defaultMaxBodyLimit. An unparseable value is reported by NewHTTP.
	MaxBodyLimit    string   `split_words:"true"`
	StaticPath      string   `split_words:"true" default:""`
	AlllowedOrigins []string `split_words:"true" default:"*"`
	// Debug makes echo's default error handler put the error a handler
	// returned into the JSON response body. Read from HTTP_DEBUG; defaults to
	// false, because those errors carry SQL text, file paths, upstream URLs
	// and connection-string credentials straight to the caller.
	Debug bool `split_words:"true"`
	// RealIPSource selects where echo.Context.RealIP() reads the client
	// address from: RealIPSourcePeer or RealIPSourceXFF. Read from
	// HTTP_REAL_IP_SOURCE; defaults to RealIPSourcePeer.
	RealIPSource string `split_words:"true"`
	// TrustedProxies lists, in CIDR notation, the hops allowed to relay a
	// client address in X-Forwarded-For. Read from HTTP_TRUSTED_PROXIES as a
	// comma-separated list, and only meaningful when RealIPSource is
	// RealIPSourceXFF. Left empty, loopback and the private ranges are
	// trusted, which covers the usual sidecar and cluster-internal ingress;
	// set, the list is exhaustive and nothing outside it is trusted.
	TrustedProxies []string `split_words:"true"`
}

func NewHttpConfig() (*HttpConfig, error) {
	var h HttpConfig
	if err := envconfig.Process("http", &h); err != nil {
		return nil, err
	}

	if h.ReadTimeout == 0 {
		h.ReadTimeout = defaultReadTimeout
	}

	if h.WriteTimeout == 0 {
		h.WriteTimeout = defaultWriteTimeout
	}

	if h.ShutdownTimeout == 0 {
		h.ShutdownTimeout = defaultShutdownTimeout
	}

	if h.StaticPath == "" {
		h.StaticPath = defaultStaticPath
	}

	if h.MaxBodyLimit == "" {
		h.MaxBodyLimit = defaultMaxBodyLimit
	}

	if h.RealIPSource == "" {
		h.RealIPSource = RealIPSourcePeer
	}

	return &h, nil
}

// bodyLimit returns the body limit to hand to the BodyLimit middleware, having
// proved the middleware can parse it. That middleware panics on a value it
// cannot read, so an operator's typo in HTTP_MAX_BODY_LIMIT would otherwise
// take down the process from inside a constructor that returns an error.
//
// An empty value means the config was built by hand without one, in which case
// the package default applies rather than the middleware's panic. A value that
// parses to zero or less is refused instead: it would reject every request
// carrying a body, and no deployment can have meant that.
func (c *HttpConfig) bodyLimit() (string, error) {
	limit := c.MaxBodyLimit
	if limit == "" {
		return defaultMaxBodyLimit, nil
	}

	size, err := bytes.Parse(limit)
	if err != nil {
		return "", fmt.Errorf("invalid max body limit %q: %w", limit, err)
	}
	if size <= 0 {
		return "", fmt.Errorf("invalid max body limit %q: parses to %d bytes, want a positive size", limit, size)
	}

	return limit, nil
}

// ipExtractor returns the echo.IPExtractor the configured source calls for.
// Echo leaves IPExtractor nil by default and falls back to legacy behaviour
// that believes X-Forwarded-For and X-Real-IP from anyone, so this is set
// explicitly in every case.
func (c *HttpConfig) ipExtractor() (echo.IPExtractor, error) {
	// Parse the ranges whatever the source is, so a malformed CIDR is
	// reported as the mistake it is rather than kept until someone switches
	// the source over and finds out in production.
	trusted, err := c.trustedProxyRanges()
	if err != nil {
		return nil, err
	}

	source := c.RealIPSource
	if source == "" {
		source = RealIPSourcePeer
	}

	switch source {
	case RealIPSourcePeer:
		// Ranges here are inert, and silently ignoring them would leave an
		// operator believing a proxy is trusted when no header is ever read.
		if len(trusted) != 0 {
			return nil, fmt.Errorf("trusted proxies are configured but the real ip source is %q, which never reads forwarding headers: set HTTP_REAL_IP_SOURCE=%s or drop HTTP_TRUSTED_PROXIES", RealIPSourcePeer, RealIPSourceXFF)
		}
		return echo.ExtractIPDirect(), nil
	case RealIPSourceXFF:
		return echo.ExtractIPFromXFFHeader(trustOptions(trusted)...), nil
	default:
		return nil, fmt.Errorf("invalid real ip source %q, want %q or %q", c.RealIPSource, RealIPSourcePeer, RealIPSourceXFF)
	}
}

// trustedProxyRanges parses TrustedProxies. Blank entries are dropped, because
// an env var set to the empty string arrives as a one-element list holding it,
// and a list written across a config file gains stray spaces.
func (c *HttpConfig) trustedProxyRanges() ([]*net.IPNet, error) {
	var ranges []*net.IPNet
	for _, entry := range c.TrustedProxies {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: %w", entry, err)
		}
		ranges = append(ranges, network)
	}

	return ranges, nil
}

// trustOptions describes which hops may relay an address in X-Forwarded-For.
//
// Echo trusts loopback, link-local and the private ranges unless told
// otherwise. Link-local is never a proxy hop in the topologies this package
// serves, so it is dropped in both branches. Named ranges are treated as the
// whole answer: an operator who lists the load balancer has said where the
// proxies are, and continuing to trust every private address as well would
// make the list decorative.
func trustOptions(trusted []*net.IPNet) []echo.TrustOption {
	if len(trusted) == 0 {
		return []echo.TrustOption{
			echo.TrustLoopback(true),
			echo.TrustPrivateNet(true),
			echo.TrustLinkLocal(false),
		}
	}

	options := []echo.TrustOption{
		echo.TrustLoopback(false),
		echo.TrustPrivateNet(false),
		echo.TrustLinkLocal(false),
	}
	for _, network := range trusted {
		options = append(options, echo.TrustIPRange(network))
	}

	return options
}
