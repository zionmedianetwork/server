package server

import (
	"fmt"
	"math"
	"net"
	"net/url"
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
	// defaultReadinessTimeout bounds one readiness probe: every registered
	// check shares it, and a check still running when it expires is reported
	// as failed. Kept well under the interval a probe is normally polled at,
	// so a wedged dependency answers 503 rather than leaving the prober to
	// time out on its own and report nothing.
	defaultReadinessTimeout = time.Second * 2
	// defaultRequestTimeout is zero: no request timeout unless a deployment
	// asks for one. See HttpConfig.RequestTimeout for why the default is off.
	defaultRequestTimeout = time.Duration(0)
	// defaultMaxBodyLimit is the largest request body accepted unless a
	// deployment raises it. The previous default, 51200M, was parsed as 51.2
	// GB of decimal megabytes: a size no process can buffer, so in practice
	// no limit at all.
	defaultMaxBodyLimit = "10M"
	// defaultCorsMaxAge is how long a browser may reuse the answer to a CORS
	// preflight before asking again. Unset, echo sends no Access-Control-Max-Age
	// at all and every non-simple cross-origin call pays for an extra OPTIONS
	// round trip.
	//
	// An hour rather than the largest value that would be accepted: browsers cap
	// this themselves — Chrome at 2 hours, Firefox at 24 — so anything above the
	// cap is silently truncated and buys nothing, while the number configured
	// here is one every current browser honours verbatim. The cache holds a
	// permission, not data: a tightened HTTP_ALLLOWED_ORIGINS or a shortened
	// method list is not seen by a browser until its cached preflight expires,
	// and an hour bounds how long a revoked origin keeps working in a tab that
	// is already open.
	//
	// The struct tag on HttpConfig.CorsMaxAge repeats this value literally
	// because an envconfig tag cannot reference a constant. Keep the two in
	// step; TestNewHttpConfigCorsSettings fails if they drift apart.
	defaultCorsMaxAge = time.Hour
	// defaultRateLimitExpiry is how long the in-memory rate limiter keeps a
	// bucket for a client address that has stopped calling. It is what stops
	// the store growing without bound: one entry per distinct client IP, each
	// dropped once it has been idle this long. Echo's own default, named here
	// so the bound is stated rather than inherited.
	defaultRateLimitExpiry = 3 * time.Minute
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
	// RequestTimeout bounds how long a handler may run before the request
	// context is cancelled and the caller is answered with 503. Read from
	// HTTP_REQUEST_TIMEOUT; defaults to defaultRequestTimeout, which is zero
	// and means no timeout.
	//
	// The default is off because this package cannot see the routes it will
	// carry. A blanket timeout is wrong for the traffic this network exists to
	// serve — large downloads and uploads, and any streaming or SSE route hold
	// a request open by design — and a library that switched one on at upgrade
	// time would convert those into 503s without anybody changing a line of
	// code. Turning it on is one variable; guessing on a consumer's behalf is
	// not recoverable.
	//
	// When set it must be strictly below WriteTimeout, or the connection is
	// severed before the 503 can be written and the timeout produces a
	// transport error instead of a response. NewHTTP refuses a value that is
	// not.
	RequestTimeout time.Duration `split_words:"true"`
	// TimeoutExemptPaths lists the routes RequestTimeout does not apply to:
	// the streaming, download and upload endpoints that are meant to run long.
	// Read from HTTP_TIMEOUT_EXEMPT_PATHS as a comma-separated list, and inert
	// while RequestTimeout is zero.
	//
	// Entries are matched against the registered route pattern rather than the
	// request URL, so a parameterised route is named by its pattern
	// ("/videos/:id/stream", not "/videos/42/stream") and the static route
	// registered by this package is "/static*". Every entry must start with a
	// slash; NewHTTP refuses one that does not.
	TimeoutExemptPaths []string `split_words:"true"`
	// ReadinessTimeout bounds a single readiness probe, covering all
	// registered checks together. Read from HTTP_READINESS_TIMEOUT; defaults
	// to defaultReadinessTimeout. A non-positive value is treated as unset.
	ReadinessTimeout time.Duration `split_words:"true"`
	// MaxBodyLimit is the largest request body accepted, written in gommon's
	// byte notation: 10M, 512K, 4MiB. Read from HTTP_MAX_BODY_LIMIT; defaults
	// to defaultMaxBodyLimit. An unparseable value is reported by NewHTTP.
	MaxBodyLimit string `split_words:"true"`
	StaticPath   string `split_words:"true" default:""`
	// AlllowedOrigins is the CORS allowlist: the origins a browser may make
	// cross-origin requests from. Read from HTTP_ALLLOWED_ORIGINS — three L's,
	// the field is misspelled and envconfig derives the variable from it — as a
	// comma-separated list. It defaults to empty, which allows no cross-origin
	// access at all.
	//
	// The default used to be "*", and that is the change to know about. A
	// wildcard default means a service that never sets the variable ships an
	// open policy for every method this package allows, including POST, PUT,
	// PATCH and DELETE, to any site that can get a browser to send the request.
	// Nothing about that is visible in a deployment's configuration, because the
	// configuration is empty. A default has to be the answer for the deployment
	// that has not thought about the question, and for cross-origin access that
	// answer is no.
	//
	// "*" remains available and is one variable away: HTTP_ALLLOWED_ORIGINS=*
	// restores the previous behaviour exactly. It is not as dangerous as it
	// looks — this package never sets Access-Control-Allow-Credentials, so a
	// wildcard cannot be used to read an authenticated response — but it is
	// still a decision, and it now has to be made rather than inherited.
	//
	// Entries name a scheme and host with no trailing slash
	// ("https://app.example.com"), optionally with a wildcard in the host
	// ("https://*.example.com"). NewHTTP refuses anything else: an origin that
	// cannot match the Origin header a browser sends would sit in the config
	// looking like an allowance that works.
	AlllowedOrigins []string `split_words:"true"`
	// AllowedHeaders is the request-header allowlist a CORS preflight is
	// answered with. Read from HTTP_ALLOWED_HEADERS — one L, unlike the origins
	// variable above — as a comma-separated list; defaults to
	// defaultAllowedHeaders.
	//
	// Left unset before, echo reflected whatever the browser asked for in
	// Access-Control-Request-Headers, which is to say it allowed every header
	// unconditionally. The default list is the set a JSON API over this package
	// actually needs, and it includes X-Request-Id on purpose: this package
	// honours a client-supplied one as the correlation id, and a browser cannot
	// send it unless the preflight says it may.
	//
	// HTTP_ALLOWED_HEADERS=* restores "any header the browser asks for". The
	// wildcard is honoured for requests without credentials, which is all this
	// package emits, with the one exception the fetch standard carves out:
	// Authorization is never covered by "*" and has to be listed by name.
	AllowedHeaders []string `split_words:"true"`
	// CorsMaxAge is how long a browser may cache a CORS preflight. Read from
	// HTTP_CORS_MAX_AGE; defaults to defaultCorsMaxAge. Zero sends no
	// Access-Control-Max-Age header, which is what this package did before and
	// makes a browser preflight every non-simple cross-origin request again.
	//
	// The default lives in the struct tag rather than in NewHttpConfig, unlike
	// most of this file, precisely so that zero stays reachable and keeps
	// meaning "off": a default applied to a zero value would leave a deployment
	// no way to ask for the old behaviour.
	//
	// The value is sent in whole seconds. NewHTTP refuses a negative duration,
	// and refuses a positive one below a second, which would truncate to zero
	// and silently turn the feature off.
	CorsMaxAge time.Duration `split_words:"true" default:"1h"`
	// DisableSecurityHeaders turns off the baseline response headers this
	// package otherwise sets on every response: X-Content-Type-Options,
	// X-Frame-Options, Referrer-Policy and X-XSS-Protection. Read from
	// HTTP_DISABLE_SECURITY_HEADERS; defaults to false, so the headers are on.
	//
	// They are on by default because they cost one map write each, they change
	// nothing for a non-browser client, and every one of them removes a class of
	// browser behaviour nobody wants from an API: content sniffed into an
	// executable type, a JSON endpoint framed by another site, a path with an id
	// in it leaked to a third party through Referer.
	//
	// The name is negative so that the zero value is the safe one. A positively
	// named bool defaulting to true could only be defaulted where the
	// environment is read, which would leave the headers off for every consumer
	// that builds an HttpConfig in Go instead.
	DisableSecurityHeaders bool `split_words:"true"`
	// HstsMaxAge is the max-age sent in Strict-Transport-Security. Read from
	// HTTP_HSTS_MAX_AGE; defaults to zero, which sends no HSTS header at all.
	//
	// Off by default, and this is the one security header that must be. HSTS is
	// a durable instruction to the browser rather than a property of one
	// response: for max-age seconds afterwards that browser refuses to speak
	// plaintext to the host, and the only way to undo it early is to serve
	// max-age=0 over working HTTPS. This process speaks H2C with no TLS of its
	// own, so whether the origin it is reached at is HTTPS everywhere is a fact
	// about a deployment's ingress that this package cannot see. Emitting HSTS
	// by default would let a library upgrade take a hostname offline for
	// whatever plaintext still depends on it.
	//
	// When it is set, echo emits the header only for requests that arrived over
	// TLS or carried X-Forwarded-Proto: https — so it also depends on the proxy
	// in front setting that header, and on that proxy being the only thing that
	// can.
	//
	// NewHTTP refuses a negative value, a positive value below a second, and a
	// value set while security headers are disabled.
	HstsMaxAge time.Duration `split_words:"true"`
	// HstsIncludeSubdomains adds includeSubdomains to the HSTS header. Read from
	// HTTP_HSTS_INCLUDE_SUBDOMAINS; defaults to false, and inert unless
	// HstsMaxAge is set — NewHTTP refuses the combination that is not, since it
	// reads as HSTS having been configured when no header is sent.
	//
	// Default false because the subdomain form is the irreversible part: it
	// pins every name under the domain, including ones this service knows
	// nothing about and ones that do not exist yet, for the whole max-age.
	HstsIncludeSubdomains bool `split_words:"true"`
	// RateLimit is the number of requests per second allowed from one client
	// address. Read from HTTP_RATE_LIMIT; defaults to zero, which is off.
	//
	// Off by default for three reasons, none of which a library can resolve on a
	// consumer's behalf. The limiter is per process, so the limit a fleet
	// actually enforces is this number times the replica count, and it changes
	// every time the deployment scales. Its store is in memory, so it holds one
	// bucket per distinct client address — bounded only by
	// defaultRateLimitExpiry, which is fine behind a proxy and is a different
	// proposition facing the open internet. And it is keyed on c.RealIP(), so it
	// inherits HTTP_REAL_IP_SOURCE: with the wrong source configured, every
	// caller shares one bucket or every caller can mint a new one at will, and a
	// limit keyed on a spoofable address is worse than no limit because it looks
	// like protection.
	//
	// Liveness and readiness paths are never rate limited. A 429 on /readyz
	// takes the instance out of load balancing and a 429 on /healthz can have an
	// orchestrator restart it, which turns a burst of traffic into an outage.
	RateLimit float64 `split_words:"true"`
	// RateLimitBurst is how many requests may arrive at once before the rate
	// applies. Read from HTTP_RATE_LIMIT_BURST; defaults to one second's worth
	// of requests at RateLimit, and never less than one — a burst of zero denies
	// every request, which is what a fractional rate would otherwise silently
	// produce.
	//
	// Inert while RateLimit is zero, and NewHTTP refuses that combination rather
	// than leaving a burst configured against a limiter that does not run.
	RateLimitBurst int `split_words:"true"`
	// Gzip compresses responses for clients that accept it. Read from HTTP_GZIP;
	// defaults to false.
	//
	// Off by default because this package carries media. Compressing an MP4, a
	// JPEG or any other already-compressed payload spends CPU on both ends to
	// produce something no smaller, and gzip buffers: a streaming or SSE route
	// only keeps streaming because echo's writer forces a gzip flush on every
	// Flush call, and the proxies in front are under no such obligation.
	//
	// When it is on, the routes named in TimeoutExemptPaths are skipped, along
	// with the probe paths. That list already names the routes a deployment has
	// declared long-running — the streams, the downloads, the uploads — which is
	// the same set compression should stay out of.
	Gzip bool `split_words:"true"`
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

	if h.ReadinessTimeout == 0 {
		h.ReadinessTimeout = defaultReadinessTimeout
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

	if len(h.AllowedHeaders) == 0 {
		h.AllowedHeaders = defaultAllowedHeaders()
	}

	return &h, nil
}

// defaultAllowedHeaders is the request-header allowlist a CORS preflight is
// answered with when nothing else is configured: what a JSON API served by this
// package needs and nothing more.
//
// Accept and Content-Type are what a browser sends for a JSON request body —
// Content-Type is the reason such a request preflights at all, since
// application/json is not one of the values a simple request may carry.
// Authorization covers the bearer token or basic credentials an API expects,
// and has to be named explicitly because the fetch standard excludes it from
// the "*" wildcard. X-Request-Id is this package's own: a client-supplied one is
// preserved as the correlation id in the access log and echoed back, which is
// useless to a browser client that is not allowed to send it.
//
// Returned as a fresh slice rather than held in a package-level variable, so a
// consumer that appends to the list it is given cannot change the default for
// the next server built in the same process.
func defaultAllowedHeaders() []string {
	return []string{
		echo.HeaderAccept,
		echo.HeaderAuthorization,
		echo.HeaderContentType,
		echo.HeaderXRequestID,
	}
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

// requestTimeout returns the bound to put on handler execution, having proved
// the bound can actually produce the response it promises.
//
// Zero means the caller never asked for one, which is the package default and
// leaves handler execution unbounded. A negative value is refused rather than
// read as "off": it would expire every request context on arrival, and nobody
// writes a negative duration meaning to disable a feature.
//
// A value at or above WriteTimeout is refused too, and this is the whole point
// of validating here. The middleware answers an expired context by writing 503,
// but net/http closes the connection the moment WriteTimeout passes, so a
// request timeout that is not strictly shorter never gets to write anything and
// the client sees a truncated stream or a reset instead of a status. Such a
// setting looks configured and does nothing but cancel handlers, which is the
// least useful half of the feature.
func (c *HttpConfig) requestTimeout() (time.Duration, error) {
	timeout := c.RequestTimeout
	if timeout == 0 {
		return defaultRequestTimeout, nil
	}
	if timeout < 0 {
		return 0, fmt.Errorf("invalid request timeout %s: want a positive duration, or zero to disable it", timeout)
	}

	// A server built by hand may carry no write timeout at all, in which case
	// nothing competes with the middleware and any positive bound works.
	if c.WriteTimeout > 0 && timeout >= c.WriteTimeout {
		return 0, fmt.Errorf("request timeout %s must be below the write timeout %s, or the connection is closed before a 503 can be written and the timeout never produces a response: lower HTTP_REQUEST_TIMEOUT or raise HTTP_WRITETIMEOUT", timeout, c.WriteTimeout)
	}

	return timeout, nil
}

// timeoutExemptPaths returns the set of route patterns the request timeout
// leaves alone. Blank entries are dropped, for the same reason they are in
// trustedProxyRanges: an env var set to the empty string arrives as a
// one-element list holding it.
//
// The list is parsed whatever RequestTimeout is, so a typo is reported when it
// is introduced rather than when someone later enables the timeout. Unlike
// trusted proxies with the wrong IP source, exemptions named while the timeout
// is off are not worth refusing: with no timeout every route is already exempt,
// so the list overstates nothing.
func (c *HttpConfig) timeoutExemptPaths() (map[string]struct{}, error) {
	exempt := make(map[string]struct{}, len(c.TimeoutExemptPaths))
	for _, entry := range c.TimeoutExemptPaths {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// Echo reports the routed path with a leading slash in every case, so
		// an entry without one can never match and would sit there looking
		// like an exemption that works.
		if !strings.HasPrefix(entry, "/") {
			return nil, fmt.Errorf("invalid timeout exempt path %q: want a route pattern beginning with %q, such as %q", entry, "/", "/"+entry)
		}
		exempt[entry] = struct{}{}
	}

	return exempt, nil
}

// corsWildcardOrigin is the entry that means "every origin". It is spelled out
// here because it is compared against, reported in errors, and is the exact
// value HTTP_ALLLOWED_ORIGINS has to carry to restore the old default.
const corsWildcardOrigin = "*"

// allowedOrigins returns the CORS allowlist, having proved every entry could
// match an Origin header. An empty result means no cross-origin access, which
// is the default and is handled explicitly by the middleware rather than left
// to echo, whose own answer to an empty list is the wildcard.
//
// Blank entries are dropped for the reason they are in trustedProxyRanges: an
// env var set to the empty string arrives as a one-element list holding it.
//
// A malformed entry is refused rather than skipped. An origin with a trailing
// slash, a path, or no scheme can never equal the Origin header a browser sends,
// so it would leave an operator reading a config that names their front end
// while the browser is refused — the same silent failure a timeout exemption
// without a leading slash produces, and it is refused for the same reason.
func (c *HttpConfig) allowedOrigins() ([]string, error) {
	origins := make([]string, 0, len(c.AlllowedOrigins))
	wildcard := false

	for _, entry := range c.AlllowedOrigins {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		if entry == corsWildcardOrigin {
			wildcard = true
			origins = append(origins, entry)
			continue
		}

		if err := validateOrigin(entry); err != nil {
			return nil, err
		}
		origins = append(origins, entry)
	}

	// The wildcard is matched first and matches everything, so a list holding it
	// alongside named origins is a list whose named entries do nothing. Left
	// alone it reads as a narrow policy and behaves as an open one, which is the
	// worst of both.
	if wildcard && len(origins) > 1 {
		return nil, fmt.Errorf("invalid allowed origins %q: %q allows every origin, so the named ones are never consulted: drop %q, or set HTTP_ALLLOWED_ORIGINS=%s on its own", c.AlllowedOrigins, corsWildcardOrigin, corsWildcardOrigin, corsWildcardOrigin)
	}

	if len(origins) == 0 {
		return nil, nil
	}

	return origins, nil
}

// validateOrigin reports whether entry is shaped like the Origin header a
// browser sends: a scheme and a host, optionally with a port, and nothing else.
// A wildcard in the host ("https://*.example.com") is echo's own pattern syntax
// and is left to it.
func validateOrigin(entry string) error {
	malformed := fmt.Errorf("invalid allowed origin %q: want a scheme and host with nothing after it, such as %q, %q or %q", entry, "https://app.example.com", "https://app.example.com:8443", "https://*.example.com")

	parsed, err := url.Parse(entry)
	if err != nil {
		return malformed
	}
	if parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return malformed
	}
	// A browser's Origin header carries no path, so "https://example.com/" —
	// which is what a copy from an address bar produces — matches nothing.
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return malformed
	}

	return nil
}

// allowedHeaders returns the request headers a preflight is answered with.
// An empty list means the config was built by hand without one, or that every
// entry was blank, in which case the package default applies — as with the body
// limit, a field nobody filled in is not an instruction to allow nothing.
func (c *HttpConfig) allowedHeaders() []string {
	headers := make([]string, 0, len(c.AllowedHeaders))
	for _, entry := range c.AllowedHeaders {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		headers = append(headers, entry)
	}

	if len(headers) == 0 {
		return defaultAllowedHeaders()
	}

	return headers
}

// corsMaxAge returns the preflight cache lifetime in the whole seconds
// Access-Control-Max-Age carries. Zero means the header is not sent.
func (c *HttpConfig) corsMaxAge() (int, error) {
	return headerSeconds(c.CorsMaxAge, "HTTP_CORS_MAX_AGE")
}

// hstsMaxAge returns the Strict-Transport-Security lifetime in whole seconds,
// or zero when no HSTS header should be sent.
//
// The two refusals here are both settings that read as HSTS being configured
// while no header is ever sent: a max age with the security headers turned off,
// and includeSubdomains with no max age to attach it to. Either one leaves an
// operator believing a host is pinned to HTTPS when nothing pins it.
func (c *HttpConfig) hstsMaxAge() (int, error) {
	maxAge, err := headerSeconds(c.HstsMaxAge, "HTTP_HSTS_MAX_AGE")
	if err != nil {
		return 0, err
	}

	if c.DisableSecurityHeaders {
		if maxAge != 0 {
			return 0, fmt.Errorf("hsts is configured with a max age of %s but security headers are disabled, so no Strict-Transport-Security header is sent: drop HTTP_DISABLE_SECURITY_HEADERS or drop HTTP_HSTS_MAX_AGE", c.HstsMaxAge)
		}
		if c.HstsIncludeSubdomains {
			return 0, fmt.Errorf("hsts subdomains are configured but security headers are disabled, so no Strict-Transport-Security header is sent: drop HTTP_DISABLE_SECURITY_HEADERS or drop HTTP_HSTS_INCLUDE_SUBDOMAINS")
		}
		return 0, nil
	}

	if maxAge == 0 && c.HstsIncludeSubdomains {
		return 0, fmt.Errorf("hsts subdomains are configured but the hsts max age is zero, so no Strict-Transport-Security header is sent: set HTTP_HSTS_MAX_AGE or drop HTTP_HSTS_INCLUDE_SUBDOMAINS")
	}

	return maxAge, nil
}

// headerSeconds converts a configured duration into the whole seconds an HTTP
// header carries, refusing the two values that would look configured and do
// nothing. name is the variable to name in the error, so the operator is told
// which setting to change rather than which field it landed in.
//
// A negative duration is refused rather than read as "off", for the reason
// requestTimeout gives: nobody writes a negative duration meaning to disable a
// feature. A positive duration under a second is refused because it truncates
// to zero, and zero is how these headers are switched off — so the shortest
// cache anyone could ask for would silently become no cache at all.
func headerSeconds(d time.Duration, name string) (int, error) {
	switch {
	case d == 0:
		return 0, nil
	case d < 0:
		return 0, fmt.Errorf("invalid %s %s: want a positive duration, or zero to disable it", name, d)
	case d < time.Second:
		return 0, fmt.Errorf("invalid %s %s: it is sent in whole seconds, so anything under a second truncates to zero and disables it: use 0 to mean that, or at least 1s", name, d)
	}

	return int(d / time.Second), nil
}

// rateLimitSettings is a resolved rate limit: how many requests per second one
// client address may make, and how many may arrive at once. A zero rate means
// no rate limiting, which is the default.
type rateLimitSettings struct {
	perSecond float64
	burst     int
}

// enabled reports whether a limiter should be installed at all.
func (r rateLimitSettings) enabled() bool {
	return r.perSecond > 0
}

// rateLimit returns the resolved limit, filling in a burst when none was asked
// for and refusing the settings that cannot work.
//
// The burst defaults to one second's worth of requests, rounded up, because the
// store echo ships treats a zero burst literally and denies every request: a
// deployment asking for half a request per second would otherwise configure an
// outage. Rounding up rather than down is what keeps a fractional rate usable
// at all.
func (c *HttpConfig) rateLimit() (rateLimitSettings, error) {
	if c.RateLimit < 0 {
		return rateLimitSettings{}, fmt.Errorf("invalid rate limit %v: want a positive number of requests per second, or zero to disable it", c.RateLimit)
	}
	if c.RateLimitBurst < 0 {
		return rateLimitSettings{}, fmt.Errorf("invalid rate limit burst %d: want a positive number of requests, or zero to derive one from HTTP_RATE_LIMIT", c.RateLimitBurst)
	}

	if c.RateLimit == 0 {
		// A burst named against a limiter that never runs is the same mistake as
		// a trusted proxy named against an IP source that reads no headers, and
		// is refused for the same reason: silently ignoring it leaves someone
		// believing requests are being limited.
		if c.RateLimitBurst != 0 {
			return rateLimitSettings{}, fmt.Errorf("a rate limit burst of %d is configured but the rate limit is off, so nothing is limited: set HTTP_RATE_LIMIT or drop HTTP_RATE_LIMIT_BURST", c.RateLimitBurst)
		}
		return rateLimitSettings{}, nil
	}

	burst := c.RateLimitBurst
	if burst == 0 {
		burst = int(math.Ceil(c.RateLimit))
	}

	return rateLimitSettings{perSecond: c.RateLimit, burst: burst}, nil
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
