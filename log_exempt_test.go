package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// The consumer's own probe routes these tests stand on. They are the ones a
// service actually registers when it does not follow this package's convention
// — /health rather than /healthz — which is the whole reason the setting
// exists.
const (
	consumerHealthPath   = "/health"
	v1ConsumerHealthPath = "/v1/health"
	consumerStatusPath   = "/status"
	// videoStatusPath is the parameterised case: an exemption names the pattern,
	// so one entry covers every id rather than one entry per video.
	videoStatusPath = "/v1/videos/:id/status"
	// healthcheckPath is the near miss. It shares a prefix with
	// v1ConsumerHealthPath and must still be logged, because a prefix match here
	// would silently take a real route out of the log.
	healthcheckPath = "/v1/healthcheck"
	// videosPath is an ordinary route nobody exempted.
	videosPath = "/v1/videos"
)

// logExemptConfig is testConfig with a consumer's own probes named in the
// access log exemption list. The blank and padded entries are there on purpose:
// an env var set to the empty string arrives as a one-element list holding it,
// and a list written across a config file gains stray spaces.
func logExemptConfig() *HttpConfig {
	cfg := testConfig()
	cfg.LogExemptPaths = []string{
		consumerHealthPath,
		v1ConsumerHealthPath,
		videoStatusPath,
		"",
		"   ",
		" " + consumerStatusPath + " ",
	}
	return cfg
}

// registerConsumerRoutes registers the routes these tests drive: the ones
// logExemptConfig names, and two it does not.
func registerConsumerRoutes(t *testing.T, s *httpServer) {
	t.Helper()

	for _, path := range []string{
		consumerHealthPath,
		v1ConsumerHealthPath,
		consumerStatusPath,
		videoStatusPath,
		healthcheckPath,
		videosPath,
	} {
		s.Echo().GET(path, func(c echo.Context) error {
			return c.NoContent(http.StatusOK)
		})
	}
}

// TestRequestLoggerExemptsConfiguredPaths covers the reason the setting exists.
// The built-in skip list can only name the four routes this package registers,
// so a consumer whose probes are at /health and /v1/health had every one of
// them in the access log — the same flood the built-in skipping prevents, from
// traffic that is polled just as hard.
//
// The negative cases are the load-bearing half. Exempting /v1/health must not
// exempt /v1/healthcheck, and naming three routes must not quieten the fourth.
func TestRequestLoggerExemptsConfiguredPaths(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		wantEntries int
	}{
		{name: "a configured path is not logged", target: consumerHealthPath, wantEntries: 0},
		{name: "a configured versioned path is not logged", target: v1ConsumerHealthPath, wantEntries: 0},
		{name: "a query string does not sneak it back in", target: consumerHealthPath + "?probe=liveness", wantEntries: 0},
		{name: "a trailing slash does not sneak it back in", target: consumerHealthPath + "/", wantEntries: 0},
		{name: "a padded entry is trimmed and still matches", target: consumerStatusPath, wantEntries: 0},
		{name: "a parameterised pattern covers every value", target: "/v1/videos/42/status", wantEntries: 0},
		{name: "the exemption is exact, not a prefix", target: healthcheckPath, wantEntries: 1},
		{name: "a route nobody exempted is logged", target: videosPath, wantEntries: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, log := newTestServerWith(t, logExemptConfig())
			registerConsumerRoutes(t, s)

			if rec := do(t, s, httptest.NewRequest(http.MethodGet, tt.target, nil)); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			if got := len(log.entriesAt(levelInfo)); got != tt.wantEntries {
				t.Errorf("logged %d entries, want %d: %+v", got, tt.wantEntries, log.recorded())
			}
		})
	}
}

// TestLogExemptPathsAreAdditiveToTheProbePaths pins the other half of the
// contract: the configured list extends the built-in one and never replaces it.
// A deployment that names /health must not thereby put /healthz back in the log,
// and a deployment that names nothing must log exactly what it logged before.
func TestLogExemptPathsAreAdditiveToTheProbePaths(t *testing.T) {
	tests := []struct {
		name    string
		exempt  []string
		skipped []string
		logged  []string
	}{
		{
			name:    "with nothing configured",
			skipped: []string{healthzPath, v1HealthzPath, readyzPath, v1ReadyzPath},
			logged:  []string{consumerHealthPath, videosPath},
		},
		{
			name:    "with the consumer's own probes configured",
			exempt:  []string{consumerHealthPath},
			skipped: []string{healthzPath, v1HealthzPath, readyzPath, v1ReadyzPath, consumerHealthPath},
			logged:  []string{videosPath},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.LogExemptPaths = tt.exempt

			s, log := newTestServerWith(t, cfg)
			registerConsumerRoutes(t, s)

			for _, path := range tt.skipped {
				do(t, s, httptest.NewRequest(http.MethodGet, path, nil))
			}
			if got := log.entriesAt(levelInfo); len(got) != 0 {
				t.Errorf("logged %d entries for %q, want none: %+v", len(got), tt.skipped, got)
			}

			for _, path := range tt.logged {
				do(t, s, httptest.NewRequest(http.MethodGet, path, nil))
			}
			if got := log.entriesAt(levelInfo); len(got) != len(tt.logged) {
				t.Errorf("logged %d entries for %q, want %d: %+v", len(got), tt.logged, len(tt.logged), got)
			}
		})
	}
}

// TestNewHTTPRejectsMalformedLogExemptPath guards the same silent failure the
// timeout list is guarded against: an entry without a leading slash can never
// match a routed path, so the route it names goes on being logged while the
// config says it is exempt.
func TestNewHTTPRejectsMalformedLogExemptPath(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
	}{
		{name: "no leading slash", paths: []string{"health"}},
		{name: "second entry is malformed", paths: []string{consumerHealthPath, "health"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.LogExemptPaths = tt.paths

			err := newHTTPError(t, cfg)
			if !strings.Contains(err.Error(), "health") {
				t.Errorf("NewHTTP() error = %v, want it to name the offending entry", err)
			}
			// The two exempt lists share a validator, so the error has to say
			// which one the entry came from or it sends an operator to the wrong
			// variable.
			if !strings.Contains(err.Error(), "log exempt path") {
				t.Errorf("NewHTTP() error = %v, want it to name the setting at fault", err)
			}
		})
	}
}

// TestLogExemptPathsIgnoresBlankEntries guards the edge of envconfig that
// TrustedProxies and TimeoutExemptPaths both have: the variable set to the
// empty string arrives as a one-element list holding it.
func TestLogExemptPathsIgnoresBlankEntries(t *testing.T) {
	cfg := testConfig()
	cfg.LogExemptPaths = []string{"", "   ", " " + consumerHealthPath + " "}

	exempt, err := cfg.logExemptPaths()
	if err != nil {
		t.Fatalf("logExemptPaths() error = %v, want nil", err)
	}
	if len(exempt) != 1 {
		t.Fatalf("logExemptPaths() = %v, want just the one real entry", exempt)
	}
	if _, ok := exempt[consumerHealthPath]; !ok {
		t.Errorf("logExemptPaths() = %v, want it to hold %q trimmed", exempt, consumerHealthPath)
	}
}

func TestNewHttpConfigLogExemptPathsSettings(t *testing.T) {
	t.Run("defaults to nothing exempt", func(t *testing.T) {
		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if len(cfg.LogExemptPaths) != 0 {
			t.Errorf("LogExemptPaths = %q, want empty by default", cfg.LogExemptPaths)
		}
	})

	t.Run("reads HTTP_LOG_EXEMPT_PATHS", func(t *testing.T) {
		t.Setenv("HTTP_LOG_EXEMPT_PATHS", consumerHealthPath+","+v1ConsumerHealthPath)

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if want := []string{consumerHealthPath, v1ConsumerHealthPath}; !slices.Equal(cfg.LogExemptPaths, want) {
			t.Errorf("LogExemptPaths = %q, want %q", cfg.LogExemptPaths, want)
		}
	})
}

// TestLogExemptPathsDoNotDisarmTheRateLimiter is the decision this setting is
// most likely to be misread as making. The probe paths are exempt from three
// things — the log, the rate limiter and the compressor — and this list feeds
// only the first. A route named in a variable called HTTP_LOG_EXEMPT_PATHS
// keeping its rate limit is a property worth a test, because the alternative is
// a config line that reads as "quieten the log" and silently removes the only
// bound on a route's traffic.
func TestLogExemptPathsDoNotDisarmTheRateLimiter(t *testing.T) {
	cfg := testConfig()
	cfg.LogExemptPaths = []string{consumerHealthPath}
	cfg.RateLimit = 1
	cfg.RateLimitBurst = 1

	s, log := newTestServerWith(t, cfg)
	registerConsumerRoutes(t, s)

	// The first request is within the burst; the second is not.
	if rec := do(t, s, httptest.NewRequest(http.MethodGet, consumerHealthPath, nil)); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec := do(t, s, httptest.NewRequest(http.MethodGet, consumerHealthPath, nil)); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request status = %d, want %d: the exemption is for the log, not the limiter", rec.Code, http.StatusTooManyRequests)
	}

	// And the 429 is not logged either, which is the cost of the exemption and
	// is stated in the docs: the route is out of the access log entirely.
	if got := log.recorded(); len(got) != 0 {
		t.Errorf("logged %d entries for an exempt path, want none: %+v", len(got), got)
	}

	// The package's own liveness path is still never limited, whatever this
	// list says.
	for i := 0; i < 3; i++ {
		if rec := do(t, s, httptest.NewRequest(http.MethodGet, healthzPath, nil)); rec.Code != http.StatusOK {
			t.Fatalf("request %d to %s status = %d, want %d", i+1, healthzPath, rec.Code, http.StatusOK)
		}
	}
}
