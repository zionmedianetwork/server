package server

// Logger is what this package needs from a logger, and all of it: three
// structured methods, which are the three it calls.
//
// It is declared here rather than imported so that using this package does not
// oblige a consumer to adopt a particular logging library. "Accept interfaces,
// and declare them where they are consumed" pays for itself directly here:
// github.com/zionmedianetwork/logam's Logger carries seventeen methods, of
// which this package calls Infow, Warnw and Errorw. Naming that type in the
// signature would have made the other fourteen — Fatal, Print, Tracef and the
// rest — the price of admission for anyone whose logger is slog, zerolog or
// logrus, and every one of them would have been written to satisfy a compiler
// and never called.
//
// Go satisfies interfaces structurally, so this is not a break for anyone: a
// logam.Logger still satisfies Logger and existing calls to NewHTTP compile
// unchanged. logam simply stops being something a consumer has to depend on.
//
// The arguments after msg are alternating keys and values — "status", 503 —
// the convention zap's SugaredLogger established and slog shares.
//
// Wiring log/slog from the standard library takes three one-line methods and
// no dependency at all:
//
//	type slogLogger struct{ l *slog.Logger }
//
//	func (s slogLogger) Infow(msg string, kv ...interface{})  { s.l.Info(msg, kv...) }
//	func (s slogLogger) Warnw(msg string, kv ...interface{})  { s.l.Warn(msg, kv...) }
//	func (s slogLogger) Errorw(msg string, kv ...interface{}) { s.l.Error(msg, kv...) }
//
//	srv, err := server.NewHTTP(nil, slogLogger{l: slog.Default()})
//
// slog takes the same alternating key/value arguments, so that adapter forwards
// them untouched; a logger with a different shape converts them in the same
// three methods. No such adapter is exported from this package. It would be six
// lines of code and a permanent piece of public API, and every consumer needs a
// slightly different one — their own handler, their own level mapping, a
// request-scoped logger pulled from a context — so the type belongs in the
// consumer, where it can be exactly what that consumer wants.
//
// This package logs through the value it is given on every request and every
// readiness probe, and installs no no-op default in its place.
type Logger interface {
	// Infow records a request that was served and a dependency that has come
	// back after failing.
	Infow(msg string, keysAndValues ...interface{})
	// Warnw records a readiness check that is failing. It is the only place the
	// cause of a failing check is written down: the probe response withholds it
	// unless Debug is set. See logReadiness for why this is warn and not error.
	Warnw(msg string, keysAndValues ...interface{})
	// Errorw records a request the server failed to serve: the handler chain
	// returned an error, or answered 5xx.
	Errorw(msg string, keysAndValues ...interface{})
}
