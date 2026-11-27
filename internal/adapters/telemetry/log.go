package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Logger returns a logger in the format format (text, json) with the level
// level (debug, info, warn, error) writing to w.
func Logger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lv slog.Level
	if level == "" {
		level = "info"
	}
	if err := lv.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return nil, fmt.Errorf("telemetry: unknown log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("telemetry: unknown log format %q (text, json)", format)
}

// Discard is a logger that writes nothing.
func Discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
