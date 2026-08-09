// Command minimal is the smallest correct service built on
// github.com/zionmedianetwork/server: configuration from the environment, two
// routes, the JSON envelope, and — the part that is easy to get wrong — a call
// site that does something with the error Run returns.
//
// Its logger is log/slog from the standard library, wired to server.Logger by
// the three one-line methods below. That is deliberate: this package asks for
// three structured methods and nothing else, so running it needs no logging
// dependency at all. See ../readiness for the same wiring with
// github.com/zionmedianetwork/logam, which satisfies server.Logger as it is.
//
// Run it with:
//
//	HTTP_BIND_ADDRESS=:8080 go run ./examples/minimal
//
// See the README next to this file for the requests to make against it.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/labstack/echo/v4"
	"github.com/zionmedianetwork/server"
)

// slogLogger is the whole adapter: server.Logger is Infow, Warnw and Errorw,
// and slog takes the same alternating key/value arguments, so each method
// forwards them untouched. A logger with a different shape — zerolog's
// fluent chain, logrus's Fields map — converts them in these same three
// methods, which is why the package declares the interface instead of
// exporting an adapter nobody's process would want unchanged.
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Infow(msg string, kv ...interface{})  { s.l.Info(msg, kv...) }
func (s slogLogger) Warnw(msg string, kv ...interface{})  { s.l.Warn(msg, kv...) }
func (s slogLogger) Errorw(msg string, kv ...interface{}) { s.l.Error(msg, kv...) }

// video is an ordinary domain object. HTTPResponse has no case for it, so it is
// answered with 200 and wrapped as {"data": ...}.
type video struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// nextID stands in for whatever a real service uses to name a new resource.
var nextID atomic.Int64

func main() {
	// The library never builds a logger. The process owns it, and every line
	// the server writes — access logs, readiness failures — goes through the
	// one it is handed here.
	//
	// A JSON handler on stdout, because the entries this package emits are
	// structured and JSON is what makes that visible (and what a log pipeline
	// wants). Swap NewJSONHandler for NewTextHandler while you are curling at
	// it if key=value is easier on the eye; nothing else changes.
	log := slogLogger{l: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))}

	if err := serve(log); err != nil {
		// Run's returned error is its only report: the package logs nothing on
		// the caller's behalf and never terminates the process. Without these
		// two lines a failed bind is completely silent and the process exits 0
		// having served nothing.
		log.Errorw("http server stopped", "error", err)
		os.Exit(1)
	}
}

// serve builds the server, registers the routes and blocks until the process is
// signalled. It returns nil once the drain has completed cleanly.
//
// The parameter is server.Logger — the interface the package declares — rather
// than a concrete logger, so the choice made in main() is the only place this
// program names one.
func serve(log server.Logger) error {
	// Read HTTP_* from the environment. NewHTTP(nil, log) does exactly this
	// internally; doing it here keeps a configuration error distinguishable
	// from a wiring one, and leaves the resolved values available to log.
	cfg, err := server.NewHttpConfig()
	if err != nil {
		return fmt.Errorf("read http configuration: %w", err)
	}

	// Everything the configuration decides is resolved and validated here: the
	// body limit, the client-address source, the request timeout against the
	// write timeout. A misconfigured server is reported now, never half-built.
	s, err := server.NewHTTP(cfg, log)
	if err != nil {
		return fmt.Errorf("build http server: %w", err)
	}

	// s.Echo() is the only route registration API — there is none on the server
	// itself. The type NewHTTP returns is unexported, so hold it in a := binding
	// rather than trying to name it in a struct field or signature.
	e := s.Echo()
	e.GET("/v1/videos/:id", getVideo)
	e.POST("/v1/videos", createVideo)

	log.Infow("http server starting",
		"bind_address", cfg.BindAddress,
		"max_body_limit", cfg.MaxBodyLimit,
		"real_ip_source", cfg.RealIPSource,
		"shutdown_timeout", cfg.ShutdownTimeout.String(),
	)

	// Run installs the SIGINT and SIGTERM handlers, serves HTTP/2 cleartext,
	// and on a signal drains in-flight requests within HTTP_SHUTDOWN_TIMEOUT
	// before returning. TLS is expected to be terminated in front of it.
	return s.Run()
}

// getVideo answers with an ordinary payload, so the envelope wraps it:
//
//	200 {"data":{"id":"42","title":"Episode 42"}}
func getVideo(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "video id is required")
	}

	return server.HTTPResponse(c, video{ID: id, Title: "Episode " + id})
}

// createVideo answers a creation with 201 and an unwrapped confirmation:
//
//	201 {"resource":"video","message":"created","id":"1"}
func createVideo(c echo.Context) error {
	var in video
	if err := c.Bind(&in); err != nil {
		// Bind's error names the offending field and its Go type, which is a
		// developer's diagnostic rather than something to hand a caller. Answer
		// with a message chosen here and attach the real cause as the internal
		// error, where the access log will pick it up.
		return echo.NewHTTPError(http.StatusBadRequest, "body must be a JSON video object").SetInternal(err)
	}
	if in.Title == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "title is required")
	}

	id := strconv.FormatInt(nextID.Add(1), 10)

	// A value here, but &server.PostConfirmation{...} answers exactly the same
	// 201 with the same body: HTTPResponse names each confirmation type twice
	// in its switch, as a value and as a pointer. It did not always — the
	// pointer form used to fall through to the envelope and answer 200 wrapped
	// in "data" — so a service still carrying a client written around that is
	// worth a look. See "Responses" in the repository README.
	return server.HTTPResponse(c, server.PostConfirmation{
		Resource: "video",
		Message:  "created",
		ID:       id,
	})
}
