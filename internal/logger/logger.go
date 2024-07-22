package logger

import (
	"context"
	"log/slog"
	"os"
)

type Logger struct {
	*slog.Logger
}

const (
	LevelDebug    = slog.LevelDebug
	LevelInfo     = slog.LevelInfo
	LevelWarning  = slog.LevelWarn
	LevelError    = slog.LevelError
	LevelCritical = slog.Level(12)
)

// ParseLevel parses a string into a log level
func ParseLevel(s string) (slog.Level, error) {
	var level slog.Level
	err := level.UnmarshalText([]byte(s))

	return level, err
}

// ErrAttr returns an attribute for an error
func (l *Logger) ErrAttr(err error) slog.Attr {
	return slog.Any("error", err)
}

// New returns a new Logger with the given log level
func New(logLevel slog.Level) *Logger {
	return &Logger{
		slog.New(
			slog.NewJSONHandler(
				os.Stderr,
				&slog.HandlerOptions{
					// AddSource: true,
					Level: logLevel,
					ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
						// Remove time from the output for predictable test output.
						//if a.Key == slog.TimeKey {
						//	return slog.Attr{}
						//}

						// Customize the name of the level key and the output string, including
						// custom level values.
						if a.Key == slog.LevelKey {
							// Rename the level key from "level" to "sev".
							// a.Key = "sev"

							// Handle custom level values.
							level := a.Value.Any().(slog.Level)

							// This could also look up the name from a map or other structure, but
							// this demonstrates using a switch statement to rename levels. For
							// maximum performance, the string values should be constants, but this
							// example uses the raw strings for readability.
							switch {
							case level < LevelInfo:
								a.Value = slog.StringValue("DEBUG")
							case level < LevelWarning:
								a.Value = slog.StringValue("INFO")
							case level < LevelError:
								a.Value = slog.StringValue("WARNING")
							case level < LevelCritical:
								a.Value = slog.StringValue("ERROR")
							default:
								a.Value = slog.StringValue("CRITICAL")
							}
						}

						return a
					},
				},
			),
		),
	}
}

// Critical logs a message at the critical level and exits the application
func (l *Logger) Critical(msg string, args ...any) {
	l.Log(context.Background(), LevelCritical, msg, args...)
	os.Exit(1)
}
