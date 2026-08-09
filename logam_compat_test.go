package server

import "github.com/zionmedianetwork/logam"

// A logam.Logger must go on satisfying Logger. Every existing consumer of this
// package passes one to NewHTTP, and the change that narrowed that parameter is
// only source-compatible for as long as this holds: the three signatures have
// to match logam's exactly, and a later edit here — msg string becoming a
// fmt.Stringer, ...interface{} becoming a []any, a fourth method appearing —
// breaks every one of those callers with no other warning.
//
// This is the whole reason the assertion lives in a test file rather than in
// logger.go. It documents and enforces the compatibility, and it still costs
// consumers nothing: nothing in the package proper imports logam, so a consumer
// building this package never compiles it and never has it in their own module
// graph. The one thing it does cost is a line in this module's go.mod, because
// Go records test dependencies of the main module alongside the rest — deleting
// this file is what would let `go mod tidy` drop logam entirely.
var _ Logger = (logam.Logger)(nil)
