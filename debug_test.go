package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// dsnError stands in for the kind of error a handler actually returns: a
// driver failure whose text names the account it tried to authenticate as.
// Nothing about it is fit to show a caller.
var dsnError = errors.New(`pq: password authentication failed for user "zion_admin"`)

// compactJSON strips the layout from a JSON body so a test can assert on what
// was said rather than how it was spaced. Debug also turns on echo's indented
// JSON, which is presentation and not the subject of these tests.
func compactJSON(t *testing.T, body string) string {
	t.Helper()

	var out bytes.Buffer
	if err := json.Compact(&out, []byte(body)); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	return out.String()
}

// TestErrorResponseHidesHandlerErrorByDefault covers C3. e.Debug was hardcoded
// to true, and echo's default error handler answers a debug server by copying
// err.Error() into the JSON body, so every handler failure narrated its cause —
// SQL text, file paths, upstream URLs, credentials out of a connection string —
// to whoever made the request.
func TestErrorResponseHidesHandlerErrorByDefault(t *testing.T) {
	s, _ := newTestServer(t)
	s.Echo().GET("/query", func(c echo.Context) error { return dsnError })

	rec := do(t, s, httptest.NewRequest(http.MethodGet, "/query", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	body := compactJSON(t, rec.Body.String())
	if want := `{"message":"Internal Server Error"}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	if strings.Contains(body, "zion_admin") || strings.Contains(body, "pq:") {
		t.Errorf("body leaks the handler error to the client: %s", body)
	}
}

// TestErrorResponseIncludesHandlerErrorWhenDebugEnabled proves the verbose
// bodies remain available to a deployment that opts in, so the default is a
// default and not a removal.
func TestErrorResponseIncludesHandlerErrorWhenDebugEnabled(t *testing.T) {
	cfg := testConfig()
	cfg.Debug = true

	s, _ := newTestServerWith(t, cfg)
	s.Echo().GET("/query", func(c echo.Context) error { return dsnError })

	rec := do(t, s, httptest.NewRequest(http.MethodGet, "/query", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a JSON object of strings: %v (%s)", err, rec.Body.String())
	}
	if got := body["error"]; got != dsnError.Error() {
		t.Errorf("body error = %q, want %q", got, dsnError.Error())
	}
}

// TestDebugAlsoControlsResponseIndentation records a second thing e.Debug
// decides: echo indents every JSON response while it is on. Turning the flag
// off is therefore visible on successful responses too, not only on errors,
// which matters to anyone comparing bodies byte for byte.
func TestDebugAlsoControlsResponseIndentation(t *testing.T) {
	tests := []struct {
		name     string
		debug    bool
		wantBody string
	}{
		{name: "off is compact", debug: false, wantBody: `{"data":{"title":"episode 1"}}`},
		{name: "on is indented", debug: true, wantBody: "{\n  \"data\": {\n    \"title\": \"episode 1\"\n  }\n}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Debug = tt.debug

			s, _ := newTestServerWith(t, cfg)
			s.Echo().GET("/video", func(c echo.Context) error {
				return HTTPResponse(c, testPayload{Title: "episode 1"})
			})

			rec := do(t, s, httptest.NewRequest(http.MethodGet, "/video", nil))

			if body := strings.TrimSpace(rec.Body.String()); body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

// TestErrorResponseBodiesByDebugSetting walks the error shapes a handler can
// produce and pins what each one tells the client in both settings. The
// message echo chose is always sent; only the underlying error is withheld.
func TestErrorResponseBodiesByDebugSetting(t *testing.T) {
	tests := []struct {
		name      string
		handler   func(echo.Context) error
		wantCode  int
		wantBody  string
		wantDebug string
	}{
		{
			name:      "plain error",
			handler:   func(c echo.Context) error { return dsnError },
			wantCode:  http.StatusInternalServerError,
			wantBody:  `{"message":"Internal Server Error"}`,
			wantDebug: `{"error":"pq: password authentication failed for user \"zion_admin\"","message":"Internal Server Error"}`,
		},
		{
			name: "http error wrapping an internal cause",
			handler: func(c echo.Context) error {
				return echo.NewHTTPError(http.StatusBadGateway, "upstream unavailable").SetInternal(dsnError)
			},
			wantCode:  http.StatusBadGateway,
			wantBody:  `{"message":"upstream unavailable"}`,
			wantDebug: `{"error":"code=502, message=upstream unavailable, internal=pq: password authentication failed for user \"zion_admin\"","message":"upstream unavailable"}`,
		},
		{
			name:      "http error with a safe message",
			handler:   func(c echo.Context) error { return echo.NewHTTPError(http.StatusNotFound, "video not found") },
			wantCode:  http.StatusNotFound,
			wantBody:  `{"message":"video not found"}`,
			wantDebug: `{"error":"code=404, message=video not found","message":"video not found"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, debug := range []bool{false, true} {
				name := "debug off"
				want := tt.wantBody
				if debug {
					name = "debug on"
					want = tt.wantDebug
				}

				t.Run(name, func(t *testing.T) {
					cfg := testConfig()
					cfg.Debug = debug

					s, _ := newTestServerWith(t, cfg)
					s.Echo().GET("/query", tt.handler)

					rec := do(t, s, httptest.NewRequest(http.MethodGet, "/query", nil))

					if rec.Code != tt.wantCode {
						t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
					}
					if body := compactJSON(t, rec.Body.String()); body != want {
						t.Errorf("body = %s, want %s", body, want)
					}
				})
			}
		})
	}
}

func TestNewHttpConfigDebug(t *testing.T) {
	t.Run("defaults to false", func(t *testing.T) {
		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if cfg.Debug {
			t.Error("Debug = true, want false by default")
		}
	})

	t.Run("reads HTTP_DEBUG", func(t *testing.T) {
		t.Setenv("HTTP_DEBUG", "true")

		cfg, err := NewHttpConfig()
		if err != nil {
			t.Fatalf("NewHttpConfig() error = %v, want nil", err)
		}
		if !cfg.Debug {
			t.Error("Debug = false, want true from HTTP_DEBUG")
		}
	})
}
