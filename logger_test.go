package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
)

// slogLogger is the adapter written out in Logger's doc comment, compiled and
// exercised here so that the example cannot quietly stop working: three
// one-line methods over log/slog, and not a mention of logam in any of them.
type slogLogger struct{ l *slog.Logger }

func (s slogLogger) Infow(msg string, kv ...interface{})  { s.l.Info(msg, kv...) }
func (s slogLogger) Warnw(msg string, kv ...interface{})  { s.l.Warn(msg, kv...) }
func (s slogLogger) Errorw(msg string, kv ...interface{}) { s.l.Error(msg, kv...) }

// lockedBuffer is the sink that adapter writes to. slog's own handlers already
// serialise their writes, but the buffer is read from the test goroutine and
// written from whichever goroutine served the request, so the lock is what
// makes that handoff safe under -race rather than incidentally correct.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// entries decodes what slog wrote: one JSON object per line, in the order the
// package logged them.
func (b *lockedBuffer) entries(t *testing.T) []map[string]interface{} {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	var decoded []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}

		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		decoded = append(decoded, entry)
	}

	return decoded
}

// wantJSONField asserts a decoded slog entry carries key with the given value.
func wantJSONField(t *testing.T, entry map[string]interface{}, key string, want interface{}) {
	t.Helper()

	got, ok := entry[key]
	if !ok {
		t.Fatalf("field %q missing from log entry: %+v", key, entry)
	}
	if got != want {
		t.Errorf("field %q = %#v, want %#v", key, got, want)
	}
}

// TestLoggerAcceptsANonLogamLogger is the point of narrowing the parameter: a
// consumer's own logger, written against server.Logger and log/slog alone,
// reaches NewHTTP and receives both kinds of entry this package emits — the
// access log from the middleware and the readiness log from the probe handler.
//
// Both are covered here because they are logged from different places by
// different methods, and it is the readiness one that has no other witness: the
// probe body withholds the cause of a failing check, so a consumer whose logger
// were not wired through would lose it entirely.
func TestLoggerAcceptsANonLogamLogger(t *testing.T) {
	sink := &lockedBuffer{}
	log := slogLogger{l: slog.New(slog.NewJSONHandler(sink, nil))}

	s, err := NewHTTP(testConfig(), log)
	if err != nil {
		t.Fatalf("NewHTTP() error = %v, want nil", err)
	}

	s.Echo().GET("/ping", func(c echo.Context) error {
		return c.String(http.StatusOK, "pong")
	})

	const dependencyError = "dial tcp 10.0.0.5:5432: connect: connection refused"
	if err := s.RegisterReadinessCheck("postgres", func(context.Context) error {
		return errors.New(dependencyError)
	}); err != nil {
		t.Fatalf("RegisterReadinessCheck() error = %v, want nil", err)
	}

	if rec := do(t, s, httptest.NewRequest(http.MethodGet, "/ping", nil)); rec.Code != http.StatusOK {
		t.Fatalf("GET /ping status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec := do(t, s, httptest.NewRequest(http.MethodGet, readyzPath, nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET %s status = %d, want %d", readyzPath, rec.Code, http.StatusServiceUnavailable)
	}

	entries := sink.entries(t)
	if len(entries) != 2 {
		t.Fatalf("logged %d entries, want 2 (one request, one readiness failure): %+v", len(entries), entries)
	}

	request := entries[0]
	wantJSONField(t, request, "level", "INFO")
	wantJSONField(t, request, "msg", requestLogMessage)
	wantJSONField(t, request, "method", http.MethodGet)
	wantJSONField(t, request, "path", "/ping")
	// JSON has one number type, so the status arrives as a float64.
	wantJSONField(t, request, "status", float64(http.StatusOK))

	readiness := entries[1]
	wantJSONField(t, readiness, "level", "WARN")
	wantJSONField(t, readiness, "msg", readinessFailedMessage)
	wantJSONField(t, readiness, "check", "postgres")
	wantJSONField(t, readiness, "error", dependencyError)
}
