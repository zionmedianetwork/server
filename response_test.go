package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// testPayload stands in for an ordinary domain object handed to HTTPResponse.
type testPayload struct {
	Title string `json:"title"`
}

// callHTTPResponse runs HTTPResponse against a throwaway context and returns the
// recorded status and body.
func callHTTPResponse(t *testing.T, data interface{}) (int, string) {
	t.Helper()

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)

	if err := HTTPResponse(c, data); err != nil {
		t.Fatalf("HTTPResponse() error = %v, want nil", err)
	}

	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestHTTPResponse(t *testing.T) {
	tests := []struct {
		name     string
		data     interface{}
		wantCode int
		wantBody string
	}{
		{
			name:     "post confirmation is created and unwrapped",
			data:     PostConfirmation{Resource: "video", Message: "created", ID: "v1"},
			wantCode: http.StatusCreated,
			wantBody: `{"resource":"video","message":"created","id":"v1"}`,
		},
		{
			name:     "patch confirmation is ok and unwrapped",
			data:     PatchConfirmation{PostConfirmation{Resource: "video", Message: "updated", ID: "v1"}},
			wantCode: http.StatusOK,
			wantBody: `{"resource":"video","message":"updated","id":"v1"}`,
		},
		{
			name:     "confirmation is ok and unwrapped",
			data:     Confirmation{Message: "deleted"},
			wantCode: http.StatusOK,
			wantBody: `{"message":"deleted"}`,
		},
		{
			name:     "arbitrary struct is ok and wrapped",
			data:     testPayload{Title: "episode 1"},
			wantCode: http.StatusOK,
			wantBody: `{"data":{"title":"episode 1"}}`,
		},
		{
			name:     "map payload is ok and wrapped",
			data:     ResponsePayload{"id": "v1"},
			wantCode: http.StatusOK,
			wantBody: `{"data":{"id":"v1"}}`,
		},
		{
			name:     "slice payload is ok and wrapped",
			data:     []string{"a", "b"},
			wantCode: http.StatusOK,
			wantBody: `{"data":["a","b"]}`,
		},
		{
			name:     "nil payload is ok and wrapped",
			data:     nil,
			wantCode: http.StatusOK,
			wantBody: `{"data":null}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := callHTTPResponse(t, tt.data)

			if code != tt.wantCode {
				t.Errorf("status = %d, want %d", code, tt.wantCode)
			}
			if body != tt.wantBody {
				t.Errorf("body = %s, want %s", body, tt.wantBody)
			}
		})
	}
}

// TestHTTPResponseCharacterizesPointerConfirmations pins down the CURRENT,
// defective behaviour of HTTPResponse when it is handed a pointer to one of the
// confirmation types. The type switch in response.go matches value types only,
// so pointers fall through to the default branch: a *PostConfirmation yields
// 200 wrapped in {"data": ...} instead of the 201 the value type produces.
//
// This test documents the bug so a fix is a deliberate, visible change; it does
// not endorse the behaviour. Update it when the pointer cases are added.
func TestHTTPResponseCharacterizesPointerConfirmations(t *testing.T) {
	tests := []struct {
		name     string
		data     interface{}
		wantCode int
		wantBody string
	}{
		{
			name:     "pointer post confirmation loses its 201",
			data:     &PostConfirmation{Resource: "video", Message: "created", ID: "v1"},
			wantCode: http.StatusOK, // should be http.StatusCreated
			wantBody: `{"data":{"resource":"video","message":"created","id":"v1"}}`,
		},
		{
			name:     "pointer patch confirmation gets wrapped",
			data:     &PatchConfirmation{PostConfirmation{Resource: "video", Message: "updated", ID: "v1"}},
			wantCode: http.StatusOK,
			wantBody: `{"data":{"resource":"video","message":"updated","id":"v1"}}`,
		},
		{
			name:     "pointer confirmation gets wrapped",
			data:     &Confirmation{Message: "deleted"},
			wantCode: http.StatusOK,
			wantBody: `{"data":{"message":"deleted"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := callHTTPResponse(t, tt.data)

			if code != tt.wantCode {
				t.Errorf("status = %d, want %d (current behaviour)", code, tt.wantCode)
			}
			if body != tt.wantBody {
				t.Errorf("body = %s, want %s (current behaviour)", body, tt.wantBody)
			}
		})
	}
}
