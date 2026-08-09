package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// panicMessage is what the panicking handler panics with. It is deliberately
// distinctive so that a test asserting it is absent from the log entry is
// asserting something real.
const panicMessage = "handler exploded"

// TestPanicIsWrittenToTheAccessLog covers M2, which was recorded as fixed and
// was not. echo's request logger calls next(c) and only then builds the entry —
// there is no defer around it — so a panic unwinding through that middleware
// skips LogValuesFunc completely. With Recover registered outside the logger,
// as it was, a panicking handler produced a 500 for the client and nothing at
// all for the injected logger: the sole record was the stack trace echo's own
// recoverer prints to stderr.
//
// With the logger outside and Recover inside, the panic never reaches the
// logger as a panic. Recover converts it into a response before returning, so
// the logger observes an ordinary request that finished with a 500.
func TestPanicIsWrittenToTheAccessLog(t *testing.T) {
	s, log := newTestServerWith(t, testConfig())
	s.Echo().GET("/panic", func(c echo.Context) error {
		panic(panicMessage)
	})

	rec := do(t, s, httptest.NewRequest(http.MethodGet, "/panic?trace=1", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	// Exactly one entry, and at error level. The level comes from the status
	// half of the rule, not the error half: see below.
	entry := onlyEntry(t, log)
	if entry.level != levelError {
		t.Errorf("level = %q, want %q (fields: %#v)", entry.level, levelError, entry.fields)
	}
	if entry.msg != requestLogMessage {
		t.Errorf("message = %q, want %q", entry.msg, requestLogMessage)
	}

	wantField(t, entry, "method", http.MethodGet)
	wantField(t, entry, "path", "/panic")
	wantField(t, entry, "uri", "/panic?trace=1")
	wantField(t, entry, "status", http.StatusInternalServerError)
	wantField(t, entry, "remote_ip", testRemoteIP)

	// This is the part that is easy to assume and wrong. echo's Recover handles
	// the recovered error itself — it calls c.Error(err) and returns nil — so
	// the logger's own `err := next(c)` is nil and there is no error to log. The
	// entry is at error level because the status is 5xx, and it carries no
	// "error" field and no trace of the panic value. The panic message reaches
	// stderr through echo's recoverer and nowhere else.
	if got, ok := entry.field("error"); ok {
		t.Errorf("field \"error\" = %#v, want it absent: Recover handles the error itself and the logger sees nil", got)
	}

	// A 500 body was written before the entry was built, so the response size is
	// the one the client got rather than the zero it would be if the values had
	// been read on the way past a panic.
	size, ok := entry.field("bytes_out")
	if !ok {
		t.Fatal("field \"bytes_out\" missing from log entry")
	}
	if got, isInt := size.(int64); !isInt || got != int64(rec.Body.Len()) {
		t.Errorf("field \"bytes_out\" = %#v, want %d", size, rec.Body.Len())
	}

	// The request id still reaches the log: RequestID is a Pre middleware, so it
	// runs outside both of the two this change reordered.
	responseID := rec.Header().Get(echo.HeaderXRequestID)
	if responseID == "" {
		t.Fatal("response is missing the X-Request-ID header")
	}
	wantField(t, entry, "request_id", responseID)

	latency, ok := entry.field("latency")
	if !ok {
		t.Fatal("field \"latency\" missing from log entry")
	}
	if latency == "" {
		t.Error("field \"latency\" is empty")
	}
}

// TestErrorHandlerPassesAreUnchangedByTheReorder counts how many times the
// error handler runs for one request, which is the question the reordering
// raises: the logger is configured with HandleError: true, so it calls c.Error
// itself for a non-nil return from next(c), and Recover — now inside it — calls
// c.Error too. If both fired on one error, a consumer's custom handler would
// run twice for a single request.
//
// The counts, measured rather than assumed, and identical either side of the
// reorder:
//
//	panic          1 pass. Recover handles the error and returns nil, so the
//	               logger's error branch is never reached. Whichever of the two
//	               is outer, the panic is handled exactly once.
//	returned error 2 passes, and this is pre-existing echo behaviour that has
//	               nothing to do with the ordering. The logger handles the
//	               error and then deliberately re-returns it — its own comment
//	               says so — for upstream middleware to see; nothing upstream
//	               consumes it, so echo's ServeHTTP hands it to the error
//	               handler a second time. That pass is a no-op: the default
//	               handler returns immediately on a committed response, which is
//	               why the status and body below are the first pass's.
//
// Counting through a wrapper is the only way to see this. Inferring it from the
// body would show nothing, precisely because the second pass writes nothing.
func TestErrorHandlerPassesAreUnchangedByTheReorder(t *testing.T) {
	tests := []struct {
		name        string
		handler     echo.HandlerFunc
		wantHandled int
		wantCode    int
		wantBody    string
	}{
		{
			name:        "a panic is handled once, by Recover",
			handler:     func(c echo.Context) error { panic(panicMessage) },
			wantHandled: 1,
			wantCode:    http.StatusInternalServerError,
			wantBody:    "{\"message\":\"Internal Server Error\"}\n",
		},
		{
			name:        "a returned error is handled by the logger and re-dispatched by echo",
			handler:     func(c echo.Context) error { return echo.NewHTTPError(http.StatusBadGateway, "upstream down") },
			wantHandled: 2,
			wantCode:    http.StatusBadGateway,
			wantBody:    "{\"message\":\"upstream down\"}\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServerWith(t, testConfig())

			e := s.Echo()
			var handled int
			e.HTTPErrorHandler = func(err error, c echo.Context) {
				handled++
				e.DefaultHTTPErrorHandler(err, c)
			}
			e.GET("/probe", tt.handler)

			rec := do(t, s, httptest.NewRequest(http.MethodGet, "/probe", nil))

			if handled != tt.wantHandled {
				t.Errorf("error handler ran %d times, want %d", handled, tt.wantHandled)
			}
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

// TestOrdinaryRequestIsUnchangedByTheRecoverReorder pins the requests that do
// not panic, which is all of them in practice. The logger now wraps a different
// span of the chain, so the fields that describe the response — status, size,
// latency — are read at a different point than they were; the returned-error
// path in particular still reaches the logger, because Recover passes an error
// return straight through and only intervenes on a panic.
func TestOrdinaryRequestIsUnchangedByTheRecoverReorder(t *testing.T) {
	tests := []struct {
		name      string
		handler   echo.HandlerFunc
		wantCode  int
		wantLevel logLevel
		wantErr   string
		wantBody  string
	}{
		{
			name:      "success",
			handler:   func(c echo.Context) error { return c.String(http.StatusOK, "pong") },
			wantCode:  http.StatusOK,
			wantLevel: levelInfo,
			wantBody:  "pong",
		},
		{
			name:      "returned http error still reaches the logger",
			handler:   func(c echo.Context) error { return echo.NewHTTPError(http.StatusBadGateway, "upstream down") },
			wantCode:  http.StatusBadGateway,
			wantLevel: levelError,
			wantErr:   "code=502, message=upstream down",
			wantBody:  "{\"message\":\"upstream down\"}\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, log := newTestServerWith(t, testConfig())
			s.Echo().GET("/probe", tt.handler)

			rec := do(t, s, httptest.NewRequest(http.MethodGet, "/probe", nil))
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}

			entry := onlyEntry(t, log)
			if entry.level != tt.wantLevel {
				t.Errorf("level = %q, want %q (fields: %#v)", entry.level, tt.wantLevel, entry.fields)
			}

			wantField(t, entry, "status", tt.wantCode)
			wantField(t, entry, "bytes_out", int64(rec.Body.Len()))
			wantField(t, entry, "remote_ip", testRemoteIP)
			wantField(t, entry, "request_id", rec.Header().Get(echo.HeaderXRequestID))

			if latency, ok := entry.field("latency"); !ok || latency == "" {
				t.Errorf("field \"latency\" = %#v, want a duration", latency)
			}

			got, ok := entry.field("error")
			if tt.wantErr == "" {
				if ok {
					t.Errorf("field \"error\" = %#v, want it absent", got)
				}
				return
			}
			if !ok {
				t.Fatalf("field \"error\" missing from log entry: %#v", entry.fields)
			}
			if got != tt.wantErr {
				t.Errorf("field \"error\" = %#v, want %#v", got, tt.wantErr)
			}
		})
	}
}
