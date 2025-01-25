package server

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

type Singular map[string]interface{}
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

func HTTPResponse(c echo.Context, data interface{}) error {
	var payload = data
	code := http.StatusOK
	switch data.(type) {
	case PostConfirmation:
		code = http.StatusCreated
	case PatchConfirmation, Confirmation:
	default:
		payload = ResponsePayload{
			"data": data,
		}
	}
	return c.JSON(code, payload)
}
