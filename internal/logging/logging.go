// Package logging builds the process-wide structured logger.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a logger writing to w. format is "text" or "json";
// level is "debug", "info", "warn" or "error".
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}

	switch strings.ToLower(format) {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q (want text or json)", format)
	}
}
