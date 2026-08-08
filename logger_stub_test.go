package server

import (
	"fmt"
	"sync"

	"github.com/zionmedianetwork/logam"
)

// logLevel identifies which logam method produced a recorded entry.
type logLevel string

const (
	levelInfo  logLevel = "info"
	levelError logLevel = "error"
	// levelFatal records calls the real logam logger would answer by calling
	// os.Exit(1). The stub records them instead so a test can assert the
	// process would have survived.
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

// stubLogger implements logam.Logger and records the structured calls the
// request logger makes. Only the *w methods are recorded; the rest satisfy the
// interface and are unused by this package.
type stubLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

var _ logam.Logger = (*stubLogger)(nil)

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

func (l *stubLogger) Errorw(msg string, keysAndValues ...interface{}) {
	l.record(levelError, msg, keysAndValues...)
}

func (l *stubLogger) Warnw(msg string, keysAndValues ...interface{})  {}
func (l *stubLogger) Debugw(msg string, keysAndValues ...interface{}) {}

// Fatal and Fatalf are recorded rather than ignored: the real logam logger
// exits the process here, so a recorded entry is proof the process would have
// died.
func (l *stubLogger) Fatal(args ...interface{}) {
	l.record(levelFatal, fmt.Sprint(args...))
}

func (l *stubLogger) Fatalf(format string, args ...interface{}) {
	l.record(levelFatal, fmt.Sprintf(format, args...))
}

func (l *stubLogger) Errorf(format string, args ...interface{}) {}
func (l *stubLogger) Error(args ...interface{})                 {}
func (l *stubLogger) Infof(format string, args ...interface{})  {}
func (l *stubLogger) Info(args ...interface{})                  {}
func (l *stubLogger) Warnf(format string, args ...interface{})  {}
func (l *stubLogger) Warn(args ...interface{})                  {}
func (l *stubLogger) Debugf(format string, args ...interface{}) {}
func (l *stubLogger) Debug(args ...interface{})                 {}
func (l *stubLogger) Printf(format string, args ...interface{}) {}
func (l *stubLogger) Print(args ...interface{})                 {}
func (l *stubLogger) Tracef(format string, args ...interface{}) {}
