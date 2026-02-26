package logger

import (
	"context"
	"log/slog"
	"os"

	slogdedup "github.com/veqryn/slog-dedup"
)

type Logger struct {
	*slog.Logger
	Level slog.Level
}

const (
	LevelDebug        = slog.LevelDebug
	LevelDebugName    = "debug"
	LevelInfo         = slog.LevelInfo
	LevelInfoName     = "info"
	LevelWarning      = slog.LevelWarn
	LevelWarningName  = "warning"
	LevelError        = slog.LevelError
	LevelErrorName    = "error"
	LevelCritical     = slog.Level(12)
	LevelCriticalName = "critical"
)

// ParseLevel parses a string into a log level.
func ParseLevel(s string) (slog.Level, error) {
	var level slog.Level

	err := level.UnmarshalText([]byte(s))

	return level, err
}

// ErrAttr returns an attribute for an error.
func ErrAttr(err error) slog.Attr {
	return slog.Any("error", err)
}

// New returns a new Logger with the given log level.
func New(logLevel slog.Level) *Logger {
	jh := slog.NewJSONHandler(
		os.Stderr,
		&slog.HandlerOptions{
			// AddSource: true,
			Level: logLevel,
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				// Customize the name of the time key.
				if a.Key == slog.TimeKey {
					a.Key = "time"
				}

				// Customize the name of the level key and the output string, including
				// custom level values.
				if a.Key == slog.LevelKey {
					// Handle custom level values.
					level := a.Value.Any().(slog.Level)

					switch {
					case level < LevelInfo:
						a.Value = slog.StringValue(LevelDebugName)
					case level < LevelWarning:
						a.Value = slog.StringValue(LevelInfoName)
					case level < LevelError:
						a.Value = slog.StringValue(LevelWarningName)
					case level < LevelCritical:
						a.Value = slog.StringValue(LevelErrorName)
					default:
						a.Value = slog.StringValue(LevelCriticalName)
					}
				}

				return a
			},
		},
	)

	overwriter := slog.New(
		slogdedup.NewOverwriteHandler(jh, nil),
	)

	slog.SetDefault(overwriter)
	slog.SetLogLoggerLevel(logLevel)

	return &Logger{
		overwriter,
		logLevel,
	}
}

// Critical logs a message at the critical level and exits the application.
func (l *Logger) Critical(msg string, args ...any) {
	l.Log(context.Background(), LevelCritical, msg, args...)
	os.Exit(1)
}
