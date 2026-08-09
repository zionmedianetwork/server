package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// largeBody is the payload the gzip tests answer with: comfortably over
// gzipMinLength, and repetitive enough that compression is unmistakable.
var largeBody = strings.Repeat("compress me ", 700)

// bodySize is its length, which every gzip assertion compares against.
var bodySize = len(largeBody)

// hardeningServer builds a server on cfg with an ordinary route, a route named
// in TimeoutExemptPaths, and a body big enough to be worth compressing.
func hardeningServer(t *testing.T, cfg *HttpConfig) *httpServer {
	t.Helper()

	s, _ := newTestServerWith(t, cfg)

	s.Echo().GET("/videos", func(c echo.Context) error {
		return c.String(http.StatusOK, largeBody)
	})
	s.Echo().GET(exemptStreamPath, func(c echo.Context) error {
		return c.String(http.StatusOK, largeBody)
	})

	return s
}

// exemptStreamPath is the route pattern the gzip tests exempt, standing in for
// the streaming and download routes a media service names.
const exemptStreamPath = "/videos/:id/stream"

// TestSecurityHeadersAreOnByDefault covers M8. None of these headers were sent
// at all, so a browser was free to sniff a content type into something
// executable, frame an endpoint from another site, or carry a path with a
// resource id in it to a third party in the Referer.
func TestSecurityHeadersAreOnByDefault(t *testing.T) {
	cfg, err := NewHttpConfig()
	if err != nil {
		t.Fatalf("NewHttpConfig() error = %v, want nil", err)
	}
	if cfg.DisableSecurityHeaders {
		t.Fatal("DisableSecurityHeaders = true, want the headers on by default")
	}

	rec := do(t, hardeningServer(t, cfg), httptest.NewRequest(http.MethodGet, "/videos", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	wantHeader(t, rec, echo.HeaderXContentTypeOptions, secureContentTypeOptions)
	wantHeader(t, rec, echo.HeaderXFrameOptions, secureFrameOptions)
	wantHeader(t, rec, echo.HeaderReferrerPolicy, secureReferrerPolicy)
	wantHeader(t, rec, echo.HeaderXXSSProtection, secureXSSProtection)

	// HSTS is the one that is not on by default: this process has no TLS of its
	// own, and the header is a durable instruction to refuse plaintext for the
	// whole host.
	wantNoHeader(t, rec, echo.HeaderStrictTransportSecurity)
}

// TestSecurityHeadersCanBeDisabled is the escape hatch, and the proof that the
// headers come from this package rather than from somewhere else in the chain.
func TestSecurityHeadersCanBeDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.DisableSecurityHeaders = true

	rec := do(t, hardeningServer(t, cfg), httptest.NewRequest(http.MethodGet, "/videos", nil))

	for _, header := range []string{
		echo.HeaderXContentTypeOptions,
		echo.HeaderXFrameOptions,
		echo.HeaderReferrerPolicy,
		echo.HeaderXXSSProtection,
	} {
		wantNoHeader(t, rec, header)
	}
}

// TestSecurityHeadersAreOnRejections is the placement claim for the outermost
// middleware in the chain. A 413 is rendered by a browser exactly like any other
// response, so it wants the same headers; a rejection that skipped them would be
// the one response where sniffing is allowed.
func TestSecurityHeadersAreOnRejections(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBodyLimit = "1K"

	s, _ := newTestServerWith(t, cfg)
	s.Echo().POST("/videos", func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/videos", bytes.NewReader(bytes.Repeat([]byte("a"), 4096)))
	rec := do(t, s, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	wantHeader(t, rec, echo.HeaderXContentTypeOptions, secureContentTypeOptions)
}

// TestHstsIsSentOnlyWhenConfiguredAndTerminated covers the header this package
// is most able to do harm with. It is off unless asked for, and even then echo
// sends it only for a request that arrived over TLS or was relayed by a proxy
// that said so — which is the only evidence this process has that HTTPS works
// for the host at all.
func TestHstsIsSentOnlyWhenConfiguredAndTerminated(t *testing.T) {
	const ninetyDays = 90 * 24 * time.Hour

	tests := []struct {
		name              string
		maxAge            time.Duration
		includeSubdomains bool
		forwardedProto    string
		want              string
	}{
		{
			name:           "off by default, even behind a terminator",
			forwardedProto: "https",
			want:           "",
		},
		{
			name:   "configured but the request was plaintext",
			maxAge: ninetyDays,
			want:   "",
		},
		{
			name:           "configured and terminated upstream",
			maxAge:         ninetyDays,
			forwardedProto: "https",
			want:           "max-age=7776000",
		},
		{
			name:              "subdomains are included only when asked",
			maxAge:            ninetyDays,
			includeSubdomains: true,
			forwardedProto:    "https",
			want:              "max-age=7776000; includeSubdomains",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.HstsMaxAge = tt.maxAge
			cfg.HstsIncludeSubdomains = tt.includeSubdomains

			req := httptest.NewRequest(http.MethodGet, "/videos", nil)
			if tt.forwardedProto != "" {
				// X-Forwarded-Proto is client-supplied, exactly like
				// X-Forwarded-For: it is only evidence when the proxy in front
				// overwrites it. That is the same requirement HTTP_REAL_IP_SOURCE
				// documents, and it applies to this header too.
				req.Header.Set(echo.HeaderXForwardedProto, tt.forwardedProto)
			}

			rec := do(t, hardeningServer(t, cfg), req)
			wantHeader(t, rec, echo.HeaderStrictTransportSecurity, tt.want)
		})
	}
}

// TestNewHTTPRejectsInertHstsSettings covers the settings that read as HSTS
// being configured while no header is ever sent. A pinned host that is not
// actually pinned is the worst of both: the risk is assumed to be handled and
// nothing handles it.
func TestNewHTTPRejectsInertHstsSettings(t *testing.T) {
	tests := []struct {
		name              string
		maxAge            time.Duration
		includeSubdomains bool
		disableHeaders    bool
		wantText          []string
	}{
		{
			name:           "a max age with the headers disabled",
			maxAge:         time.Hour,
			disableHeaders: true,
			wantText:       []string{"HTTP_DISABLE_SECURITY_HEADERS", "HTTP_HSTS_MAX_AGE"},
		},
		{
			name:              "subdomains with the headers disabled",
			includeSubdomains: true,
			disableHeaders:    true,
			wantText:          []string{"HTTP_DISABLE_SECURITY_HEADERS", "HTTP_HSTS_INCLUDE_SUBDOMAINS"},
		},
		{
			name:              "subdomains with no max age",
			includeSubdomains: true,
			wantText:          []string{"HTTP_HSTS_MAX_AGE", "HTTP_HSTS_INCLUDE_SUBDOMAINS"},
		},
		{
			name:     "negative is not a way to disable it",
			maxAge:   -time.Hour,
			wantText: []string{"-1h0m0s", "positive"},
		},
		{
			name:     "under a second truncates to off",
			maxAge:   900 * time.Millisecond,
			wantText: []string{"900ms", "HTTP_HSTS_MAX_AGE"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.HstsMaxAge = tt.maxAge
			cfg.HstsIncludeSubdomains = tt.includeSubdomains
			cfg.DisableSecurityHeaders = tt.disableHeaders

			err := newHTTPError(t, cfg)
			for _, want := range tt.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("NewHTTP() error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

// rateLimitedConfig is testConfig with the tightest limit that still serves a
// request: one per second, one at a time. The second request in a test arrives
// microseconds after the first, so the bucket is provably empty rather than
// racing a refill.
func rateLimitedConfig() *HttpConfig {
	cfg := testConfig()
	cfg.RateLimit = 1
	return cfg
}

// requestFrom is a GET from a named client address, which is what the limiter
// keys on.
func requestFrom(target, ip string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = net.JoinHostPort(ip, "51234")
	return req
}

// TestRateLimitIsOffByDefault pins the deliberate default. The limiter is per
// process, its store grows with distinct client addresses, and it is keyed on
// c.RealIP() — three properties a library cannot resolve for a deployment it
// cannot see, so it does not guess.
func TestRateLimitIsOffByDefault(t *testing.T) {
	cfg, err := NewHttpConfig()
	if err != nil {
		t.Fatalf("NewHttpConfig() error = %v, want nil", err)
	}
	if cfg.RateLimit != 0 {
		t.Fatalf("RateLimit = %v, want 0", cfg.RateLimit)
	}

	s := hardeningServer(t, cfg)
	for i := 0; i < 50; i++ {
		if rec := do(t, s, httptest.NewRequest(http.MethodGet, "/videos", nil)); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want %d with no rate limit configured", i, rec.Code, http.StatusOK)
		}
	}
}

// TestRateLimitAnswers429WhenEnabled is the failure mode the feature exists for,
// and the three properties that make it usable: the limit is per client address,
// the refusal is logged, and the probe endpoints are never refused.
func TestRateLimitAnswers429WhenEnabled(t *testing.T) {
	s, log := newTestServerWith(t, rateLimitedConfig())
	s.Echo().GET("/videos", func(c echo.Context) error {
		return c.String(http.StatusOK, "listed")
	})

	if rec := do(t, s, requestFrom("/videos", "198.51.100.10")); rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want %d", rec.Code, http.StatusOK)
	}
	rec := do(t, s, requestFrom("/videos", "198.51.100.10"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	// Another caller has its own bucket. A limiter that counted every client
	// together would take one noisy caller and refuse the rest of the internet.
	if rec := do(t, s, requestFrom("/videos", "198.51.100.11")); rec.Code != http.StatusOK {
		t.Errorf("second client: status = %d, want %d", rec.Code, http.StatusOK)
	}

	// A limit that does not reach the log cannot be distinguished from a client
	// that stopped calling, which is why the limiter sits inside the access log
	// rather than outside it.
	var refusals int
	for _, entry := range log.recorded() {
		if status, ok := entry.field("status"); ok && status == http.StatusTooManyRequests {
			refusals++
		}
	}
	if refusals != 1 {
		t.Errorf("logged %d refusals, want exactly 1: %+v", refusals, log.recorded())
	}

	// Probes are exempt. A 429 on /readyz withdraws the instance from load
	// balancing and a 429 on /healthz invites a restart, so a burst of client
	// traffic would otherwise take the process down through its own probes.
	for _, path := range []string{healthzPath, readyzPath} {
		for i := 0; i < 5; i++ {
			if rec := do(t, s, requestFrom(path, "198.51.100.10")); rec.Code != http.StatusOK {
				t.Errorf("%s request %d: status = %d, want %d", path, i, rec.Code, http.StatusOK)
			}
		}
	}
}

// TestRateLimitRefusalIsLegibleToABrowser is the other half of the CORS
// placement: a 429 handed to a browser without Access-Control-Allow-Origin is a
// refusal the script cannot read, so a rate-limited client would report a CORS
// failure instead of "slow down".
func TestRateLimitRefusalIsLegibleToABrowser(t *testing.T) {
	cfg := rateLimitedConfig()
	cfg.AlllowedOrigins = []string{allowedOrigin}

	s := corsServer(t, cfg)

	first := simpleRequest(allowedOrigin)
	first.RemoteAddr = net.JoinHostPort("198.51.100.20", "51234")
	if rec := do(t, s, first); rec.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want %d", rec.Code, http.StatusOK)
	}

	second := simpleRequest(allowedOrigin)
	second.RemoteAddr = net.JoinHostPort("198.51.100.20", "51234")

	rec := do(t, s, second)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	wantHeader(t, rec, echo.HeaderAccessControlAllowOrigin, allowedOrigin)
}

// TestRateLimitBurstIsDerivedWhenUnset covers the setting that would otherwise
// configure an outage: echo's store treats a zero burst literally and refuses
// everything, so a fractional rate needs a burst of its own.
func TestRateLimitBurstIsDerivedWhenUnset(t *testing.T) {
	tests := []struct {
		name      string
		rate      float64
		burst     int
		wantBurst int
	}{
		{name: "whole rate", rate: 20, burst: 0, wantBurst: 20},
		{name: "fractional rate rounds up", rate: 0.5, burst: 0, wantBurst: 1},
		{name: "a configured burst is used", rate: 20, burst: 100, wantBurst: 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.RateLimit = tt.rate
			cfg.RateLimitBurst = tt.burst

			limit, err := cfg.rateLimit()
			if err != nil {
				t.Fatalf("rateLimit() error = %v, want nil", err)
			}
			if !limit.enabled() {
				t.Fatalf("rateLimit() reported disabled for a rate of %v", tt.rate)
			}
			if limit.burst != tt.wantBurst {
				t.Errorf("burst = %d, want %d", limit.burst, tt.wantBurst)
			}
		})
	}
}

// TestFractionalRateLimitStillServesARequest is the same claim end to end: half
// a request per second must serve the first one, not refuse it.
func TestFractionalRateLimitStillServesARequest(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimit = 0.5

	s := hardeningServer(t, cfg)
	if rec := do(t, s, requestFrom("/videos", "198.51.100.30")); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewHTTPRejectsInvalidRateLimit(t *testing.T) {
	tests := []struct {
		name     string
		rate     float64
		burst    int
		wantText []string
	}{
		{name: "negative rate", rate: -1, wantText: []string{"-1", "positive"}},
		{name: "negative burst", rate: 10, burst: -5, wantText: []string{"-5", "HTTP_RATE_LIMIT"}},
		{name: "a burst with no rate", burst: 100, wantText: []string{"100", "HTTP_RATE_LIMIT_BURST"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.RateLimit = tt.rate
			cfg.RateLimitBurst = tt.burst

			err := newHTTPError(t, cfg)
			for _, want := range tt.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("NewHTTP() error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestNewHTTPRejectsInvalidRateLimitFromEnv(t *testing.T) {
	t.Setenv("HTTP_RATE_LIMIT_BURST", "200")

	err := newHTTPError(t, nil)
	if !strings.Contains(err.Error(), "HTTP_RATE_LIMIT") {
		t.Errorf("NewHTTP() error = %v, want it to name the variable that is missing", err)
	}
}

// gzipRequest is a GET that accepts compression, which is the only condition
// under which the middleware does anything.
func gzipRequest(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set(echo.HeaderAcceptEncoding, "gzip")
	return req
}

// TestGzipIsOffByDefault pins the default. This package carries media, and
// compressing an already-compressed payload spends CPU on both ends to produce
// something no smaller.
func TestGzipIsOffByDefault(t *testing.T) {
	cfg, err := NewHttpConfig()
	if err != nil {
		t.Fatalf("NewHttpConfig() error = %v, want nil", err)
	}
	if cfg.Gzip {
		t.Fatal("Gzip = true, want it off by default")
	}

	rec := do(t, hardeningServer(t, cfg), gzipRequest("/videos"))

	wantNoHeader(t, rec, echo.HeaderContentEncoding)
	if rec.Body.Len() < bodySize {
		t.Errorf("body = %d bytes, want the uncompressed %d", rec.Body.Len(), bodySize)
	}
}

// TestGzipCompressesWhenEnabled covers the feature and the three cases it stays
// out of: a response too small to be worth it, a route the deployment declared
// long-running, and the probe endpoints.
func TestGzipCompressesWhenEnabled(t *testing.T) {
	cfg := testConfig()
	cfg.Gzip = true
	cfg.TimeoutExemptPaths = []string{exemptStreamPath}

	s := hardeningServer(t, cfg)
	s.Echo().GET("/version", func(c echo.Context) error {
		return c.String(http.StatusOK, "v1")
	})

	t.Run("a large response is compressed", func(t *testing.T) {
		rec := do(t, s, gzipRequest("/videos"))

		wantHeader(t, rec, echo.HeaderContentEncoding, "gzip")
		if rec.Body.Len() >= bodySize {
			t.Errorf("body = %d bytes, want it smaller than the uncompressed %d", rec.Body.Len(), bodySize)
		}

		// The bytes have to be readable as gzip, not merely labelled as it.
		reader, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("gzip.NewReader() error = %v, want a readable stream", err)
		}
		defer reader.Close()

		decompressed, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("reading the compressed body failed: %v", err)
		}
		if len(decompressed) != bodySize {
			t.Errorf("decompressed body = %d bytes, want %d", len(decompressed), bodySize)
		}
	})

	t.Run("a small response is left alone", func(t *testing.T) {
		rec := do(t, s, gzipRequest("/version"))

		wantNoHeader(t, rec, echo.HeaderContentEncoding)
		if got := rec.Body.String(); got != "v1" {
			t.Errorf("body = %q, want %q", got, "v1")
		}
	})

	t.Run("a route exempt from the request timeout is left alone", func(t *testing.T) {
		// Compression buffers, and the exemption list is where a deployment
		// names the routes that stream, download and upload.
		rec := do(t, s, gzipRequest("/videos/42/stream"))

		wantNoHeader(t, rec, echo.HeaderContentEncoding)
		if rec.Body.Len() != bodySize {
			t.Errorf("body = %d bytes, want the uncompressed %d", rec.Body.Len(), bodySize)
		}
	})

	t.Run("probe paths are left alone", func(t *testing.T) {
		rec := do(t, s, gzipRequest(healthzPath))

		wantNoHeader(t, rec, echo.HeaderContentEncoding)
		if got := rec.Body.String(); got != http.StatusText(http.StatusOK) {
			t.Errorf("body = %q, want %q", got, http.StatusText(http.StatusOK))
		}
	})

	t.Run("a client that does not accept gzip gets none", func(t *testing.T) {
		rec := do(t, s, httptest.NewRequest(http.MethodGet, "/videos", nil))

		wantNoHeader(t, rec, echo.HeaderContentEncoding)
		if rec.Body.Len() != bodySize {
			t.Errorf("body = %d bytes, want the uncompressed %d", rec.Body.Len(), bodySize)
		}
	})
}

func TestNewHttpConfigHardeningSettings(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.DisableSecurityHeaders {
			t.Error("DisableSecurityHeaders = true, want false")
		}
		if cfg.HstsMaxAge != 0 {
			t.Errorf("HstsMaxAge = %s, want 0", cfg.HstsMaxAge)
		}
		if cfg.HstsIncludeSubdomains {
			t.Error("HstsIncludeSubdomains = true, want false")
		}
		if cfg.RateLimit != 0 || cfg.RateLimitBurst != 0 {
			t.Errorf("RateLimit = %v, RateLimitBurst = %d, want both 0", cfg.RateLimit, cfg.RateLimitBurst)
		}
		if cfg.Gzip {
			t.Error("Gzip = true, want false")
		}
	})

	t.Run("reads the hardening variables", func(t *testing.T) {
		t.Setenv("HTTP_DISABLE_SECURITY_HEADERS", "true")
		t.Setenv("HTTP_HSTS_MAX_AGE", "720h")
		t.Setenv("HTTP_HSTS_INCLUDE_SUBDOMAINS", "true")
		t.Setenv("HTTP_RATE_LIMIT", "12.5")
		t.Setenv("HTTP_RATE_LIMIT_BURST", "40")
		t.Setenv("HTTP_GZIP", "true")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if !cfg.DisableSecurityHeaders {
			t.Error("DisableSecurityHeaders = false, want true")
		}
		if want := 720 * time.Hour; cfg.HstsMaxAge != want {
			t.Errorf("HstsMaxAge = %s, want %s", cfg.HstsMaxAge, want)
		}
		if !cfg.HstsIncludeSubdomains {
			t.Error("HstsIncludeSubdomains = false, want true")
		}
		if cfg.RateLimit != 12.5 {
			t.Errorf("RateLimit = %v, want 12.5", cfg.RateLimit)
		}
		if cfg.RateLimitBurst != 40 {
			t.Errorf("RateLimitBurst = %d, want 40", cfg.RateLimitBurst)
		}
		if !cfg.Gzip {
			t.Error("Gzip = false, want true")
		}
	})
}
