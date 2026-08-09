// Command streaming is the media-service case: a request timeout tight enough
// to protect the ordinary API, and a streaming route that must not be killed by
// it.
//
// Three routes, deliberately different:
//
//   - /v1/videos/:id/stream is exempt from the timeout and writes chunks for
//     ten seconds.
//   - /v1/videos/:id is not exempt, propagates the request context into its
//     (simulated) dependency call, and is answered 503 when the deadline
//     passes.
//   - /v1/videos/:id/transcode is not exempt either but ignores its context. It
//     shows the limit of the feature: cancellation is a message, not a stop.
//
// Run it with the settings the three routes are written against:
//
//	HTTP_BIND_ADDRESS=:8080 \
//	HTTP_REQUEST_TIMEOUT=2s \
//	HTTP_TIMEOUT_EXEMPT_PATHS=/v1/videos/:id/stream \
//	HTTP_WRITETIMEOUT=120s \
//	go run ./examples/streaming
//
// HTTP_WRITETIMEOUT is not decoration. net/http closes the connection once the
// write timeout passes regardless of what any middleware thinks, so with the
// default 15s the exempt stream would still be cut off at fifteen seconds, and
// the exemption would look broken rather than misconfigured. Exempting a route
// from the request timeout and leaving the write timeout alone is the most
// common way to get this wrong. Note also the spelling: HTTP_WRITETIMEOUT, not
// HTTP_WRITE_TIMEOUT.
//
// See the README next to this file for the requests to make against it.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/zionmedianetwork/server"
)

// slogLogger adapts log/slog to server.Logger, the same three one-line methods
// as in ../minimal. This example logs at both levels the access log uses — info
// for the stream that completes, error for the route the timeout cuts off — so
// it is also where you can see that the level rule is the package's and not a
// property of any particular logging library.
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Infow(msg string, kv ...interface{})  { s.l.Info(msg, kv...) }
func (s slogLogger) Warnw(msg string, kv ...interface{})  { s.l.Warn(msg, kv...) }
func (s slogLogger) Errorw(msg string, kv ...interface{}) { s.l.Error(msg, kv...) }

const (
	// streamPath is the route pattern, and it is the pattern — not a request
	// URL — that HTTP_TIMEOUT_EXEMPT_PATHS has to name. "/v1/videos/42/stream"
	// as an exemption matches nothing and silently leaves the route timed out.
	streamPath = "/v1/videos/:id/stream"

	// The stream writes chunkCount chunks chunkInterval apart, so it runs for
	// about ten seconds: comfortably past a 2s request timeout, comfortably
	// inside a 120s write timeout.
	chunkCount    = 10
	chunkInterval = time.Second

	// metadataLatency is how long the simulated dependency behind the ordinary
	// route takes. Longer than a 2s request timeout, so that route is the one
	// the timeout is there for.
	metadataLatency = 5 * time.Second
)

// videoMeta is the payload the ordinary route would answer with, if it ever got
// the chance.
type videoMeta struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Duration string `json:"duration"`
}

func main() {
	log := slogLogger{l: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))}

	if err := serve(log); err != nil {
		log.Errorw("http server stopped", "error", err)
		os.Exit(1)
	}
}

func serve(log server.Logger) error {
	cfg, err := server.NewHttpConfig()
	if err != nil {
		return fmt.Errorf("read http configuration: %w", err)
	}

	s, err := server.NewHTTP(cfg, log)
	if err != nil {
		// NewHTTP refuses a request timeout at or above the write timeout: the
		// connection would be closed before the 503 could be written, so the
		// timeout would cancel handlers and never produce a response. It also
		// refuses an exempt path without a leading slash.
		return fmt.Errorf("build http server: %w", err)
	}

	e := s.Echo()
	e.GET(streamPath, streamVideo)
	e.GET("/v1/videos/:id", videoMetadata)
	e.GET("/v1/videos/:id/transcode", transcodeVideo)

	log.Infow("http server starting",
		"bind_address", cfg.BindAddress,
		"request_timeout", cfg.RequestTimeout.String(),
		"timeout_exempt_paths", cfg.TimeoutExemptPaths,
		"write_timeout", cfg.WriteTimeout.String(),
	)
	if cfg.RequestTimeout == 0 {
		log.Warnw("no request timeout is configured, so every route runs unbounded",
			"variable", "HTTP_REQUEST_TIMEOUT",
		)
	}

	return s.Run()
}

// streamVideo is the long-running route. It is exempt from the request timeout,
// so its request context carries no deadline at all — but it still watches that
// context, because the client hanging up cancels it and there is no reason to
// keep producing bytes nobody will read.
func streamVideo(c echo.Context) error {
	ctx := c.Request().Context()

	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/plain; charset=utf-8")
	// Committing the response here is what makes this a stream rather than a
	// slow reply: the status goes out now and the body follows in pieces.
	res.WriteHeader(http.StatusOK)

	ticker := time.NewTicker(chunkInterval)
	defer ticker.Stop()

	for chunk := 1; chunk <= chunkCount; chunk++ {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			// The response is already committed, so there is no status left to
			// send and echo's error handler will not try. Returning the error
			// stops the work and puts the reason in the access log, which is
			// the only place it can still be reported.
			return fmt.Errorf("stream abandoned after %d chunks: %w", chunk-1, ctx.Err())
		}

		if _, err := fmt.Fprintf(res, "chunk %d of %d\n", chunk, chunkCount); err != nil {
			return fmt.Errorf("write chunk %d: %w", chunk, err)
		}
		// Without the flush the chunks sit in the buffer and the "stream"
		// arrives all at once at the end.
		res.Flush()
	}

	return nil
}

// videoMetadata is the ordinary route the request timeout exists to protect. It
// propagates the request context into the call that can block, which is what
// makes the timeout real, and hands the context's error back, which is what
// turns it into a 503 rather than a hung request.
func videoMetadata(c echo.Context) error {
	ctx := c.Request().Context()

	if err := lookupMetadata(ctx); err != nil {
		// The timeout middleware answers an error that wraps
		// context.DeadlineExceeded with 503 Service Unavailable. Wrapping with
		// %w keeps errors.Is working, so the wrapping does not cost the 503.
		return fmt.Errorf("look up video metadata: %w", err)
	}

	id := c.Param("id")
	return server.HTTPResponse(c, videoMeta{ID: id, Title: "Episode " + id, Duration: "00:42:10"})
}

// transcodeVideo is the counter-example, and it is here on purpose.
//
// It ignores its request context, so nothing about the timeout applies to it:
// the deadline passes, the middleware is not told, and the handler runs to
// completion holding its goroutine and its connection exactly as it would with
// no timeout configured at all. The client waits for the full duration and then
// gets a 200 — or, if the work outlasts HTTP_WRITETIMEOUT, gets its connection
// closed with no status at all.
//
// A handler like this is why the package documents the timeout as a bound on
// handlers that cooperate rather than a bound on requests. The fix is not a
// setting; it is passing ctx into whatever blocks.
func transcodeVideo(c echo.Context) error {
	// time.Sleep is the smallest possible stand-in for the real offender: a
	// database call, an HTTP request or an exec that was never given a context.
	time.Sleep(4 * time.Second)

	id := c.Param("id")
	return server.HTTPResponse(c, server.Confirmation{
		Message: fmt.Sprintf("transcode of video %s finished, late and unbothered", id),
	})
}

// lookupMetadata simulates a dependency call that honours the context it is
// given: sql.DB.QueryContext, an http.Request built with context, a gRPC call.
func lookupMetadata(ctx context.Context) error {
	timer := time.NewTimer(metadataLatency)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
