package server

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// ResponsePayload is the envelope this package wraps an ordinary payload in,
// and a convenient type for building an ad-hoc one by hand.
type ResponsePayload map[string]interface{}

type PostConfirmation struct {
	Resource string `json:"resource"`
	Message  string `json:"message"`
	ID       string `json:"id"`
}

type PatchConfirmation struct {
	PostConfirmation
}

type Confirmation struct {
	Message string `json:"message"`
}

// HTTPResponse writes data as JSON: a confirmation as itself, under the status
// that confirmation means, and anything else with 200 under a "data" key.
//
// A pointer to a confirmation is treated exactly as the value is, so
// HTTPResponse(c, &PostConfirmation{...}) and HTTPResponse(c, PostConfirmation{...})
// produce the same 201 and the same body. They did not until now: the type
// switch matched value types only, so the pointer form — which is what anyone
// building the confirmation in a variable first ends up writing — fell through
// to the envelope and answered 200 wrapped in "data". Nothing about the call
// site said so, and the difference is only visible in the response.
//
// A nil pointer is not a confirmation. A typed nil carries no resource, no id
// and no message, so answering 201 with a body of null would report a creation
// that has nothing in it; it takes the envelope instead and comes out as
// 200 {"data":null}, which is what an untyped nil has always produced.
func HTTPResponse(c echo.Context, data interface{}) error {
	if code, bare := confirmationStatus(data); bare {
		return c.JSON(code, data)
	}

	return c.JSON(http.StatusOK, ResponsePayload{
		"data": data,
	})
}

// confirmationStatus reports the status a confirmation payload is answered
// with, and whether data is one at all. The status is meaningless when the
// second result is false, and is not read.
//
// Each type appears twice, as a value and as a pointer, and that repetition is
// the point: the set of payloads answered bare is exactly the three named here
// in exactly two forms, whatever a consumer builds out of them. The obvious
// alternative — an unexported method on the three types, asserted for as an
// interface — collapses the six cases into three and covers both forms for
// free, but a method is promoted by embedding, and this file embeds:
// PatchConfirmation would inherit PostConfirmation's 201 unless it overrode it,
// and a consumer's `struct{ server.PostConfirmation; ... }` would silently
// start answering 201 bare where it is answered 200 wrapped today. A type
// switch matches concrete types and promotes nothing, so neither can happen.
// That is worth three extra cases.
//
// A nil pointer is reported as not a confirmation. It is checked here rather
// than left to encoding/json because the alternative is a 201 whose body is
// null; see HTTPResponse.
//
// Adding a confirmation type means adding both of its cases. One without the
// other is the defect this function was fixed for.
func confirmationStatus(data interface{}) (int, bool) {
	switch v := data.(type) {
	case PostConfirmation:
		return http.StatusCreated, true
	case *PostConfirmation:
		return http.StatusCreated, v != nil
	case PatchConfirmation:
		return http.StatusOK, true
	case *PatchConfirmation:
		return http.StatusOK, v != nil
	case Confirmation:
		return http.StatusOK, true
	case *Confirmation:
		return http.StatusOK, v != nil
	default:
		return 0, false
	}
}
