// Package logging builds the process's logger.
//
// It is a small package so that every binary in this repository logs the same
// way, and so the level is chosen in exactly one place.
package logging

import (
	"io"
	"log/slog"
)

// New returns a text logger at the named level.
//
// It is a text handler rather than JSON because the primary reader is a person
// looking at a GitHub Actions log or `kubectl logs`, where a JSON line per
// event is harder to scan than it is to parse.
func New(level string, w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: parseLevel(level)}))
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
