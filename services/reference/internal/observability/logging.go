package observability

import (
	"log/slog"
	"os"
)

// NewLogger returns a JSON-encoding slog logger at the given level.
// Structured logs are the default per §6.6.
func NewLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     level,
		AddSource: false,
	}))
}
