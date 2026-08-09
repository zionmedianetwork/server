package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	gommonbytes "github.com/labstack/gommon/bytes"
)

// TestDefaultMaxBodyLimitIsEnforceable covers H1. The old default, 51200M, is
// read by gommon as decimal megabytes: 51.2 GB, more than any process can
// buffer, so the middleware was configured never to reject anything. The new
// default is a size a service can actually absorb.
func TestDefaultMaxBodyLimitIsEnforceable(t *testing.T) {
	const wantBytes = 10 * 1000 * 1000

	size, err := gommonbytes.Parse(defaultMaxBodyLimit)
	if err != nil {
		t.Fatalf("bytes.Parse(%q) error = %v, want nil", defaultMaxBodyLimit, err)
	}
	if size != wantBytes {
		t.Errorf("default max body limit = %d bytes, want %d", size, wantBytes)
	}

	// The value the old default stood for, kept here so the size of the gap is
	// on the record rather than in a commit message.
	old, err := gommonbytes.Parse("51200M")
	if err != nil {
		t.Fatalf("bytes.Parse(%q) error = %v, want nil", "51200M", err)
	}
	if old <= size {
		t.Errorf("old default parsed to %d bytes, want it far above the new %d", old, size)
	}
}

func TestNewHttpConfigMaxBodyLimit(t *testing.T) {
	t.Run("defaults to ten megabytes", func(t *testing.T) {
		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.MaxBodyLimit != defaultMaxBodyLimit {
			t.Errorf("MaxBodyLimit = %q, want %q", cfg.MaxBodyLimit, defaultMaxBodyLimit)
		}
	})

	t.Run("reads HTTP_MAX_BODY_LIMIT", func(t *testing.T) {
		t.Setenv("HTTP_MAX_BODY_LIMIT", "32M")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.MaxBodyLimit != "32M" {
			t.Errorf("MaxBodyLimit = %q, want %q", cfg.MaxBodyLimit, "32M")
		}
	})
}

// TestBodyOverLimitIsRejected proves the configured limit is the one actually
// applied, in both of the ways the middleware can notice: a declared
// Content-Length, and a body that turns out to be longer than it claimed.
func TestBodyOverLimitIsRejected(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBodyLimit = "1K"

	tests := []struct {
		name     string
		size     int
		wantCode int
	}{
		{name: "under the limit is served", size: 512, wantCode: http.StatusOK},
		{name: "at the limit is served", size: 1000, wantCode: http.StatusOK},
		{name: "over the limit is rejected", size: 1001, wantCode: http.StatusRequestEntityTooLarge},
		{name: "far over the limit is rejected", size: 64 * 1024, wantCode: http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServerWith(t, cfg)
			s.Echo().POST("/upload", func(c echo.Context) error {
				return c.NoContent(http.StatusOK)
			})

			body := bytes.Repeat([]byte("a"), tt.size)
			req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))

			rec := do(t, s, req)
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d for a %d byte body", rec.Code, tt.wantCode, tt.size)
			}
		})
	}
}

// TestBodyOverLimitIsRejectedWithoutContentLength covers the second check in
// the middleware: a client that does not declare a length is still cut off
// once it has sent more than the limit allows.
func TestBodyOverLimitIsRejectedWithoutContentLength(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBodyLimit = "1K"

	s, _ := newTestServerWith(t, cfg)
	s.Echo().POST("/upload", func(c echo.Context) error {
		if _, err := c.Request().Body.Read(make([]byte, 4096)); err != nil {
			return err
		}
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(strings.Repeat("a", 8*1024)))
	req.ContentLength = -1

	if rec := do(t, s, req); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

// TestNewHTTPRejectsUnparseableBodyLimit covers M6. middleware.BodyLimit
// answers a value it cannot parse by panicking, so before this a typo in
// HTTP_MAX_BODY_LIMIT killed the process from inside a constructor that
// already had an error to return. newHTTPError fails the test if a panic
// escapes at all.
func TestNewHTTPRejectsUnparseableBodyLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit string
	}{
		{name: "typo in the unit", limit: "10MM"},
		{name: "not a number", limit: "ten megabytes"},
		{name: "unit only", limit: "M"},
		{name: "negative size", limit: "-10M"},
		{name: "zero", limit: "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.MaxBodyLimit = tt.limit

			err := newHTTPError(t, cfg)
			if !strings.Contains(err.Error(), tt.limit) {
				t.Errorf("NewHTTP() error = %v, want it to name the value %q", err, tt.limit)
			}
		})
	}
}

// TestNewHTTPRejectsUnparseableBodyLimitFromEnv walks the same failure in
// through the env, which is how an operator will actually cause it.
func TestNewHTTPRejectsUnparseableBodyLimitFromEnv(t *testing.T) {
	t.Setenv("HTTP_MAX_BODY_LIMIT", "10 megs")

	err := newHTTPError(t, nil)
	if !strings.Contains(err.Error(), "10 megs") {
		t.Errorf("NewHTTP() error = %v, want it to name the value %q", err, "10 megs")
	}
}

// TestBodyLimitFallsBackToDefault mirrors shutdownTimeout's treatment of a
// hand-built config: a field left unset means the caller never chose, so the
// package default applies rather than the middleware's panic.
func TestBodyLimitFallsBackToDefault(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		want       string
	}{
		{name: "unset falls back", configured: "", want: defaultMaxBodyLimit},
		{name: "configured value is used", configured: "4M", want: "4M"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.MaxBodyLimit = tt.configured

			got, err := cfg.bodyLimit()
			if err != nil {
				t.Fatalf("bodyLimit() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("bodyLimit() = %q, want %q", got, tt.want)
			}
		})
	}
}
