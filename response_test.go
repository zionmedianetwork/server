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
			name:     "pointer to an arbitrary struct is ok and wrapped",
			data:     &testPayload{Title: "episode 1"},
			wantCode: http.StatusOK,
			wantBody: `{"data":{"title":"episode 1"}}`,
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

// TestHTTPResponsePointerConfirmationsMatchValues covers all six forms a
// confirmation can be handed to HTTPResponse in — three types, by value and by
// pointer — and asserts the two forms are indistinguishable in the response.
//
// It used to assert the opposite. Until this change the type switch in
// response.go matched value types only, so a pointer fell through to the
// default branch and a *PostConfirmation answered 200 wrapped in {"data": ...}
// where the value answered 201 bare. That was characterised here rather than
// fixed, and the characterisation is what this replaces: one expectation per
// type, shared by both forms, so a pointer case dropped from the switch fails
// against the value's own expected status and body rather than quietly
// establishing a second, worse behaviour.
func TestHTTPResponsePointerConfirmationsMatchValues(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		pointer  interface{}
		wantCode int
		wantBody string
	}{
		{
			name:     "post confirmation is created and unwrapped",
			value:    PostConfirmation{Resource: "video", Message: "created", ID: "v1"},
			pointer:  &PostConfirmation{Resource: "video", Message: "created", ID: "v1"},
			wantCode: http.StatusCreated,
			wantBody: `{"resource":"video","message":"created","id":"v1"}`,
		},
		{
			name:     "patch confirmation is ok and unwrapped",
			value:    PatchConfirmation{PostConfirmation{Resource: "video", Message: "updated", ID: "v1"}},
			pointer:  &PatchConfirmation{PostConfirmation{Resource: "video", Message: "updated", ID: "v1"}},
			wantCode: http.StatusOK,
			wantBody: `{"resource":"video","message":"updated","id":"v1"}`,
		},
		{
			name:     "confirmation is ok and unwrapped",
			value:    Confirmation{Message: "deleted"},
			pointer:  &Confirmation{Message: "deleted"},
			wantCode: http.StatusOK,
			wantBody: `{"message":"deleted"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forms := []struct {
				name string
				data interface{}
			}{
				{name: "value", data: tt.value},
				{name: "pointer", data: tt.pointer},
			}

			for _, form := range forms {
				t.Run(form.name, func(t *testing.T) {
					code, body := callHTTPResponse(t, form.data)

					if code != tt.wantCode {
						t.Errorf("status = %d, want %d", code, tt.wantCode)
					}
					if body != tt.wantBody {
						t.Errorf("body = %s, want %s", body, tt.wantBody)
					}
				})
			}
		})
	}
}

// TestHTTPResponseTypedNilConfirmations covers the case the pointer fix creates
// and that a response path must never answer with a panic: a declared but
// unassigned confirmation pointer.
//
//	var confirmation *PostConfirmation
//	return server.HTTPResponse(c, confirmation)
//
// That value is not an untyped nil — it carries a type, so it reaches the
// pointer cases in the switch — and a naive fix would either dereference it or
// answer 201 with a body of null, reporting a creation with nothing in it.
// It takes the envelope instead and comes out exactly as an untyped nil does.
//
// The test fails on a panic without an assertion for it: an unrecovered panic
// in HTTPResponse fails the test binary here, which is the whole point of
// covering it.
func TestHTTPResponseTypedNilConfirmations(t *testing.T) {
	tests := []struct {
		name string
		data interface{}
	}{
		{name: "nil post confirmation", data: (*PostConfirmation)(nil)},
		{name: "nil patch confirmation", data: (*PatchConfirmation)(nil)},
		{name: "nil confirmation", data: (*Confirmation)(nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := callHTTPResponse(t, tt.data)

			if code != http.StatusOK {
				t.Errorf("status = %d, want %d: a nil pointer confirms nothing, so it is not a confirmation", code, http.StatusOK)
			}
			if want := `{"data":null}`; body != want {
				t.Errorf("body = %s, want %s", body, want)
			}
		})
	}
}
