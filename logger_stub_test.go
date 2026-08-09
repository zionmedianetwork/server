package server

import (
	"fmt"
	"sync"
)

// logLevel identifies which Logger method produced a recorded entry.
type logLevel string

const (
	levelInfo  logLevel = "info"
	levelWarn  logLevel = "warn"
	levelError logLevel = "error"
	// levelFatal records a Fatal call, which the real logam logger answers by
	// calling os.Exit(1). The stub records it instead so a test can assert the
	// process would have survived. Nothing in this package can reach it through
	// server.Logger, which has no Fatal — see the assertion below.
	levelFatal logLevel = "fatal"
)

// logEntry is a single structured call recorded by stubLogger.
type logEntry struct {
	level  logLevel
	msg    string
	fields map[string]interface{}
}

// field returns the value logged under key and whether it was present.
func (e logEntry) field(key string) (interface{}, bool) {
	v, ok := e.fields[key]
	return v, ok
}

// stubLogger implements Logger and records the structured calls this package
// makes, so a test can read back what was logged.
type stubLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

var _ Logger = (*stubLogger)(nil)

// The stub deliberately carries two methods Logger does not, asserted here so
// that it reads as intent rather than as leftovers from the wider interface
// this stub used to implement. Fatal and Fatalf are the ways a logger ends the
// process, and the shutdown tests assert through wantNoFatal that a graceful
// shutdown never reaches them.
//
// Narrowing the injected logger to Logger has already settled that question the
// stronger way: the package cannot call a method its interface does not
// declare, so wantNoFatal now checks something the compiler guarantees. The
// four lines stay because they cost four lines, and because they make that
// check live again on the day somebody widens Logger.
var _ interface {
	Fatal(args ...interface{})
	Fatalf(format string, args ...interface{})
} = (*stubLogger)(nil)

func (l *stubLogger) record(level logLevel, msg string, keysAndValues ...interface{}) {
	fields := make(map[string]interface{}, len(keysAndValues)/2)
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		key, ok := keysAndValues[i].(string)
		if !ok {
			continue
		}
		fields[key] = keysAndValues[i+1]
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, logEntry{level: level, msg: msg, fields: fields})
}

// recorded returns a snapshot of every entry logged so far.
func (l *stubLogger) recorded() []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]logEntry(nil), l.entries...)
}

// entriesAt returns every entry recorded at the given level.
func (l *stubLogger) entriesAt(level logLevel) []logEntry {
	var matched []logEntry
	for _, e := range l.recorded() {
		if e.level == level {
			matched = append(matched, e)
		}
	}
	return matched
}

func (l *stubLogger) Infow(msg string, keysAndValues ...interface{}) {
	l.record(levelInfo, msg, keysAndValues...)
}

// Warnw is recorded because readiness failures are reported there: the cause
// of a failing check is deliberately kept out of the response body, so the log
// entry is the only place it exists and a test has to be able to read it.
func (l *stubLogger) Warnw(msg string, keysAndValues ...interface{}) {
	l.record(levelWarn, msg, keysAndValues...)
}

func (l *stubLogger) Errorw(msg string, keysAndValues ...interface{}) {
	l.record(levelError, msg, keysAndValues...)
}

func (l *stubLogger) Fatal(args ...interface{}) {
	l.record(levelFatal, fmt.Sprint(args...))
}

func (l *stubLogger) Fatalf(format string, args ...interface{}) {
	l.record(levelFatal, fmt.Sprintf(format, args...))
}
