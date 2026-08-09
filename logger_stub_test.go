package server

import "sync"

// logLevel identifies which Logger method produced a recorded entry.
type logLevel string

const (
	levelInfo  logLevel = "info"
	levelWarn  logLevel = "warn"
	levelError logLevel = "error"
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
