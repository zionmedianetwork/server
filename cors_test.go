package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	// allowedOrigin is the front end a test deployment has named, and
	// deniedOrigin is the site that has not been named and must be refused.
	allowedOrigin = "https://app.example.com"
	deniedOrigin  = "https://evil.example.net"
)

// corsConfig is testConfig with an origin allowed and preflight caching at the
// package default, which is the shape of a deployment that has configured CORS.
func corsConfig() *HttpConfig {
	cfg := testConfig()
	cfg.AlllowedOrigins = []string{allowedOrigin}
	cfg.CorsMaxAge = defaultCorsMaxAge
	return cfg
}

// corsServer builds a server on cfg with one route per method the tests need.
func corsServer(t *testing.T, cfg *HttpConfig) *httpServer {
	t.Helper()

	s, _ := newTestServerWith(t, cfg)
	s.Echo().GET("/videos", func(c echo.Context) error {
		return c.String(http.StatusOK, "listed")
	})
	s.Echo().POST("/videos", func(c echo.Context) error {
		return c.String(http.StatusOK, "created")
	})

	return s
}

// simpleRequest is a cross-origin GET: the request a browser sends without a
// preflight, and the one whose response headers decide whether the script that
// asked for it may read the body.
func simpleRequest(origin string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/videos", nil)
	req.Header.Set(echo.HeaderOrigin, origin)
	return req
}

// preflightRequest is the OPTIONS a browser sends before a cross-origin POST
// carrying JSON.
func preflightRequest(origin string) *http.Request {
	req := httptest.NewRequest(http.MethodOptions, "/videos", nil)
	req.Header.Set(echo.HeaderOrigin, origin)
	req.Header.Set(echo.HeaderAccessControlRequestMethod, http.MethodPost)
	req.Header.Set(echo.HeaderAccessControlRequestHeaders, echo.HeaderContentType)
	return req
}

// wantHeader asserts a response header has exactly the given value.
func wantHeader(t *testing.T, rec *httptest.ResponseRecorder, name, want string) {
	t.Helper()

	if got := rec.Header().Get(name); got != want {
		t.Errorf("header %q = %q, want %q", name, got, want)
	}
}

// wantNoHeader asserts a response header was not sent at all.
func wantNoHeader(t *testing.T, rec *httptest.ResponseRecorder, name string) {
	t.Helper()

	if got := rec.Header().Get(name); got != "" {
		t.Errorf("header %q = %q, want it absent", name, got)
	}
}

// TestCorsDefaultAllowsNoOrigin covers H4. The default was "*", so any service
// that never set HTTP_ALLLOWED_ORIGINS published every route — including POST,
// PUT, PATCH and DELETE — to any site that could get a browser to call it, and
// nothing in that deployment's configuration said so. The default now allows
// nothing, and a deployment that wants the old behaviour asks for it.
func TestCorsDefaultAllowsNoOrigin(t *testing.T) {
	cfg, err := NewHttpConfig()
	if err != nil {
		t.Fatalf("NewHttpConfig() error = %v, want nil", err)
	}
	if len(cfg.AlllowedOrigins) != 0 {
		t.Fatalf("AlllowedOrigins = %q, want empty by default", cfg.AlllowedOrigins)
	}

	s := corsServer(t, cfg)

	t.Run("a simple request gets no allow-origin", func(t *testing.T) {
		rec := do(t, s, simpleRequest(allowedOrigin))

		// The request is still served: this is a browser policy, and a
		// server-to-server caller that happens to send an Origin header is not
		// something this package refuses.
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Body.String(); got != "listed" {
			t.Errorf("body = %q, want %q", got, "listed")
		}
		// Without this header the browser withholds the response from the
		// script that asked for it, which is the whole mechanism.
		wantNoHeader(t, rec, echo.HeaderAccessControlAllowOrigin)
	})

	t.Run("a preflight is refused", func(t *testing.T) {
		rec := do(t, s, preflightRequest(allowedOrigin))

		wantNoHeader(t, rec, echo.HeaderAccessControlAllowOrigin)
		wantNoHeader(t, rec, echo.HeaderAccessControlAllowMethods)
		wantNoHeader(t, rec, echo.HeaderAccessControlAllowHeaders)
	})
}

// TestCorsAllowsOnlyNamedOrigins is the pair of claims that matter: an origin
// the deployment named is answered with permissive headers, and one it did not
// is answered with none.
func TestCorsAllowsOnlyNamedOrigins(t *testing.T) {
	cfg := corsConfig()
	cfg.AlllowedOrigins = []string{allowedOrigin, "https://*.trusted.example.com"}

	s := corsServer(t, cfg)

	tests := []struct {
		name   string
		origin string
		want   string
	}{
		{name: "the named origin is allowed", origin: allowedOrigin, want: allowedOrigin},
		{name: "a subdomain pattern is allowed", origin: "https://admin.trusted.example.com", want: "https://admin.trusted.example.com"},
		{name: "another origin is refused", origin: deniedOrigin, want: ""},
		{name: "the wrong scheme is refused", origin: "http://app.example.com", want: ""},
		{name: "the wrong port is refused", origin: allowedOrigin + ":8443", want: ""},
		{name: "the apex behind the pattern is refused", origin: "https://trusted.example.com.evil.net", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, s, simpleRequest(tt.origin))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			wantHeader(t, rec, echo.HeaderAccessControlAllowOrigin, tt.want)

			// A preflight has to agree with the simple request, or a browser
			// would be told it may send a POST and then be refused the answer.
			pre := do(t, s, preflightRequest(tt.origin))
			wantHeader(t, pre, echo.HeaderAccessControlAllowOrigin, tt.want)
			if tt.want == "" {
				wantNoHeader(t, pre, echo.HeaderAccessControlAllowMethods)
				return
			}
			wantHeader(t, pre, echo.HeaderAccessControlAllowMethods, "GET,POST,PUT,PATCH,DELETE")
		})
	}
}

// TestCorsCredentialsAreNeverAllowed pins the property that keeps even the
// wildcard short of catastrophic: this package never sends
// Access-Control-Allow-Credentials, so a cross-origin script cannot use the
// caller's cookies to read an authenticated response.
func TestCorsCredentialsAreNeverAllowed(t *testing.T) {
	cfg := corsConfig()
	cfg.AlllowedOrigins = []string{corsWildcardOrigin}

	s := corsServer(t, cfg)

	rec := do(t, s, simpleRequest(deniedOrigin))
	wantHeader(t, rec, echo.HeaderAccessControlAllowOrigin, corsWildcardOrigin)
	wantNoHeader(t, rec, echo.HeaderAccessControlAllowCredentials)
}

// TestCorsWildcardRestoresThePreviousDefault is the exact escape hatch the
// behaviour change promises. Anyone whose browser clients depended on the old
// wildcard sets one variable and is back where they were.
func TestCorsWildcardRestoresThePreviousDefault(t *testing.T) {
	t.Setenv("HTTP_ALLLOWED_ORIGINS", "*")

	cfg, err := NewHttpConfig()
	if err != nil {
		t.Fatalf("NewHttpConfig() error = %v, want nil", err)
	}

	s := corsServer(t, cfg)

	for _, origin := range []string{allowedOrigin, deniedOrigin} {
		rec := do(t, s, simpleRequest(origin))
		wantHeader(t, rec, echo.HeaderAccessControlAllowOrigin, corsWildcardOrigin)
	}
}

// TestCorsPreflightCarriesNonZeroMaxAge covers P1. With MaxAge unset echo sent
// no Access-Control-Max-Age at all, so a browser preflighted every non-simple
// cross-origin call and paid a round trip per request.
func TestCorsPreflightCarriesNonZeroMaxAge(t *testing.T) {
	t.Run("the default is sent in whole seconds", func(t *testing.T) {
		rec := do(t, corsServer(t, corsConfig()), preflightRequest(allowedOrigin))

		want := strconv.Itoa(int(defaultCorsMaxAge / time.Second))
		wantHeader(t, rec, echo.HeaderAccessControlMaxAge, want)

		// The defect was a zero, which instructs a browser not to cache at all,
		// so a value that merely exists is not enough.
		if want == "0" {
			t.Fatalf("defaultCorsMaxAge = %s, want a duration that survives the conversion to seconds", defaultCorsMaxAge)
		}
	})

	t.Run("a configured value is used", func(t *testing.T) {
		cfg := corsConfig()
		cfg.CorsMaxAge = 10 * time.Minute

		rec := do(t, corsServer(t, cfg), preflightRequest(allowedOrigin))
		wantHeader(t, rec, echo.HeaderAccessControlMaxAge, "600")
	})

	t.Run("zero restores the previous behaviour", func(t *testing.T) {
		cfg := corsConfig()
		cfg.CorsMaxAge = 0

		rec := do(t, corsServer(t, cfg), preflightRequest(allowedOrigin))
		wantNoHeader(t, rec, echo.HeaderAccessControlMaxAge)
	})

	t.Run("a refused origin gets no max age either", func(t *testing.T) {
		rec := do(t, corsServer(t, corsConfig()), preflightRequest(deniedOrigin))
		wantNoHeader(t, rec, echo.HeaderAccessControlMaxAge)
	})
}

// TestCorsPreflightAllowsTheConfiguredHeaders covers the second half of H4: with
// AllowHeaders unset, echo reflected whatever the browser asked for, which is an
// allowlist that allows everything.
func TestCorsPreflightAllowsTheConfiguredHeaders(t *testing.T) {
	t.Run("the default list is sent, not the requested one", func(t *testing.T) {
		req := preflightRequest(allowedOrigin)
		req.Header.Set(echo.HeaderAccessControlRequestHeaders, "X-Admin-Override")

		rec := do(t, corsServer(t, corsConfig()), req)

		want := strings.Join(defaultAllowedHeaders(), ",")
		wantHeader(t, rec, echo.HeaderAccessControlAllowHeaders, want)
		if strings.Contains(rec.Header().Get(echo.HeaderAccessControlAllowHeaders), "X-Admin-Override") {
			t.Error("the requested header was reflected back, want only the configured allowlist")
		}
	})

	t.Run("the correlation header is allowed", func(t *testing.T) {
		// A client-supplied X-Request-ID is preserved as the correlation id, so
		// a browser that cannot send it loses the only tie between its own
		// trace and this server's access log.
		if !slices.Contains(defaultAllowedHeaders(), echo.HeaderXRequestID) {
			t.Errorf("defaultAllowedHeaders() = %q, want it to allow %q", defaultAllowedHeaders(), echo.HeaderXRequestID)
		}
	})

	t.Run("a configured list is used", func(t *testing.T) {
		cfg := corsConfig()
		cfg.AllowedHeaders = []string{"Content-Type", " X-Tenant ", ""}

		rec := do(t, corsServer(t, cfg), preflightRequest(allowedOrigin))
		wantHeader(t, rec, echo.HeaderAccessControlAllowHeaders, "Content-Type,X-Tenant")
	})

	t.Run("a wildcard restores the previous behaviour", func(t *testing.T) {
		cfg := corsConfig()
		cfg.AllowedHeaders = []string{corsWildcardOrigin}

		rec := do(t, corsServer(t, cfg), preflightRequest(allowedOrigin))
		wantHeader(t, rec, echo.HeaderAccessControlAllowHeaders, corsWildcardOrigin)
	})
}

// TestCorsHeadersAreOnRejectedRequests is the placement claim. CORS used to sit
// inside the body limit, so a browser upload over the limit was answered 413
// with no Access-Control-Allow-Origin — and a response a browser will not hand
// to the script that asked for it has no status and no body as far as that
// script is concerned. The upload looked like a CORS misconfiguration instead of
// a file that was too big.
func TestCorsHeadersAreOnRejectedRequests(t *testing.T) {
	cfg := corsConfig()
	cfg.MaxBodyLimit = "1K"

	s := corsServer(t, cfg)
	s.Echo().GET("/panics", func(c echo.Context) error {
		panic("boom")
	})

	tests := []struct {
		name     string
		request  func() *http.Request
		wantCode int
	}{
		{
			name: "a body over the limit",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/videos", bytes.NewReader(bytes.Repeat([]byte("a"), 4096)))
				req.Header.Set(echo.HeaderOrigin, allowedOrigin)
				return req
			},
			wantCode: http.StatusRequestEntityTooLarge,
		},
		{
			name: "a recovered panic",
			request: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/panics", nil)
				req.Header.Set(echo.HeaderOrigin, allowedOrigin)
				return req
			},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, s, tt.request())

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			wantHeader(t, rec, echo.HeaderAccessControlAllowOrigin, allowedOrigin)
			// The other outermost middleware, on the same response, for the
			// same reason.
			wantHeader(t, rec, echo.HeaderXContentTypeOptions, secureContentTypeOptions)
		})
	}
}

// TestNewHTTPRejectsMalformedOrigins guards the silent failure: an origin that
// cannot equal the Origin header a browser sends is an allowance that never
// fires, and it would sit in the configuration looking like one that does.
func TestNewHTTPRejectsMalformedOrigins(t *testing.T) {
	tests := []struct {
		name     string
		origins  []string
		wantText string
	}{
		{name: "no scheme", origins: []string{"app.example.com"}, wantText: "app.example.com"},
		{name: "trailing slash", origins: []string{"https://app.example.com/"}, wantText: "https://app.example.com/"},
		{name: "a path", origins: []string{"https://app.example.com/videos"}, wantText: "/videos"},
		{name: "a query string", origins: []string{"https://app.example.com?tenant=1"}, wantText: "tenant=1"},
		{name: "credentials in the authority", origins: []string{"https://user:pass@app.example.com"}, wantText: "user:pass"},
		{name: "no host", origins: []string{"https://"}, wantText: "https://"},
		{name: "the second entry is malformed", origins: []string{allowedOrigin, "example.org"}, wantText: "example.org"},
		{name: "the wildcard with named origins", origins: []string{corsWildcardOrigin, allowedOrigin}, wantText: "never consulted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.AlllowedOrigins = tt.origins

			err := newHTTPError(t, cfg)
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("NewHTTP() error = %v, want it to mention %q", err, tt.wantText)
			}
		})
	}
}

func TestNewHTTPRejectsMalformedOriginsFromEnv(t *testing.T) {
	t.Setenv("HTTP_ALLLOWED_ORIGINS", "https://app.example.com,app.example.org")

	err := newHTTPError(t, nil)
	if !strings.Contains(err.Error(), "app.example.org") {
		t.Errorf("NewHTTP() error = %v, want it to name the offending entry", err)
	}
}

// TestNewHTTPRejectsUnusableCorsMaxAge covers the setting that looks configured
// and does nothing: Access-Control-Max-Age is whole seconds, so anything under a
// second truncates to zero, which is how the header is switched off.
func TestNewHTTPRejectsUnusableCorsMaxAge(t *testing.T) {
	tests := []struct {
		name     string
		maxAge   time.Duration
		wantText []string
	}{
		{name: "negative", maxAge: -time.Second, wantText: []string{"-1s", "positive"}},
		{name: "under a second", maxAge: 500 * time.Millisecond, wantText: []string{"500ms", "HTTP_CORS_MAX_AGE"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := corsConfig()
			cfg.CorsMaxAge = tt.maxAge

			err := newHTTPError(t, cfg)
			for _, want := range tt.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("NewHTTP() error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

// TestAllowedOriginsIgnoresBlankEntries guards the same edge of envconfig that
// TrustedProxies and TimeoutExemptPaths do: the variable set to the empty string
// arrives as a one-element list holding it, and that must read as "no origins"
// rather than as an origin named "".
func TestAllowedOriginsIgnoresBlankEntries(t *testing.T) {
	t.Run("blank entries in a hand-built config", func(t *testing.T) {
		cfg := testConfig()
		cfg.AlllowedOrigins = []string{"", "   ", " " + allowedOrigin + " "}

		origins, err := cfg.allowedOrigins()
		if err != nil {
			t.Fatalf("allowedOrigins() error = %v, want nil", err)
		}
		if want := []string{allowedOrigin}; !slices.Equal(origins, want) {
			t.Errorf("allowedOrigins() = %q, want %q", origins, want)
		}
	})

	t.Run("the variable set to nothing allows nothing", func(t *testing.T) {
		t.Setenv("HTTP_ALLLOWED_ORIGINS", "")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}

		rec := do(t, corsServer(t, cfg), simpleRequest(allowedOrigin))
		wantNoHeader(t, rec, echo.HeaderAccessControlAllowOrigin)
	})
}

func TestNewHttpConfigCorsSettings(t *testing.T) {
	t.Run("defaults to no origins, the default headers and an hour of caching", func(t *testing.T) {
		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if len(cfg.AlllowedOrigins) != 0 {
			t.Errorf("AlllowedOrigins = %q, want empty", cfg.AlllowedOrigins)
		}
		if !slices.Equal(cfg.AllowedHeaders, defaultAllowedHeaders()) {
			t.Errorf("AllowedHeaders = %q, want %q", cfg.AllowedHeaders, defaultAllowedHeaders())
		}
		// The struct tag carries this default because zero has to stay
		// reachable and keep meaning "send no header", so the constant and the
		// tag are two statements of one value and can drift apart.
		if cfg.CorsMaxAge != defaultCorsMaxAge {
			t.Errorf("CorsMaxAge = %s, want %s: the `default` tag and defaultCorsMaxAge disagree", cfg.CorsMaxAge, defaultCorsMaxAge)
		}
	})

	t.Run("reads HTTP_ALLLOWED_ORIGINS, HTTP_ALLOWED_HEADERS and HTTP_CORS_MAX_AGE", func(t *testing.T) {
		t.Setenv("HTTP_ALLLOWED_ORIGINS", allowedOrigin+",https://admin.example.com")
		t.Setenv("HTTP_ALLOWED_HEADERS", "Content-Type,X-Tenant")
		t.Setenv("HTTP_CORS_MAX_AGE", "30m")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if want := []string{allowedOrigin, "https://admin.example.com"}; !slices.Equal(cfg.AlllowedOrigins, want) {
			t.Errorf("AlllowedOrigins = %q, want %q", cfg.AlllowedOrigins, want)
		}
		if want := []string{"Content-Type", "X-Tenant"}; !slices.Equal(cfg.AllowedHeaders, want) {
			t.Errorf("AllowedHeaders = %q, want %q", cfg.AllowedHeaders, want)
		}
		if want := 30 * time.Minute; cfg.CorsMaxAge != want {
			t.Errorf("CorsMaxAge = %s, want %s", cfg.CorsMaxAge, want)
		}
	})

	t.Run("HTTP_CORS_MAX_AGE=0 is honoured rather than defaulted", func(t *testing.T) {
		t.Setenv("HTTP_CORS_MAX_AGE", "0")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.CorsMaxAge != 0 {
			t.Errorf("CorsMaxAge = %s, want 0: zero is how preflight caching is switched off", cfg.CorsMaxAge)
		}
	})
}
