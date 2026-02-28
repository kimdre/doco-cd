package logger

import (
	"errors"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		level   string
		want    slog.Level
		wantErr bool
	}{
		{
			name:    "debug",
			level:   "debug",
			want:    LevelDebug,
			wantErr: false,
		},
		{
			name:    "info",
			level:   "info",
			want:    LevelInfo,
			wantErr: false,
		},
		{
			name:    "warn",
			level:   "warn",
			want:    LevelWarning,
			wantErr: false,
		},
		{
			name:    "error",
			level:   "error",
			want:    LevelError,
			wantErr: false,
		},
		{
			name:    "invalid",
			level:   "invalid",
			want:    LevelInfo,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseLevel(tt.level)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseLevel() error = %v, wantErr %v", err, tt.wantErr)

				return
			}

			if got != tt.want {
				t.Errorf("ParseLevel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestErrAttr(t *testing.T) {
	t.Parallel()

	err := errors.New("test message")

	attr := ErrAttr(err)
	if attr.Key != "error" {
		t.Errorf("ErrAttr() key = %v, want %v", attr.Key, "error")
	}

	if !attr.Equal(slog.Any("error", err)) {
		t.Errorf("ErrAttr() value = %v, want %v", attr.Value, err)
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	logLevel := LevelDebug
	logger := New(logLevel)

	if logger.Level != logLevel {
		t.Errorf("New() level = %v, want %v", logger.Level, logLevel)
	}
}

func TestLogger_ParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		level   string
		want    slog.Level
		wantErr bool
	}{
		{
			name:    "Debug",
			level:   "debug",
			want:    LevelDebug,
			wantErr: false,
		},
		{
			name:    "INFO",
			level:   "info",
			want:    LevelInfo,
			wantErr: false,
		},
		{
			name:    "warn",
			level:   "warn",
			want:    LevelWarning,
			wantErr: false,
		},
		{
			name:    "ERRor",
			level:   "error",
			want:    LevelError,
			wantErr: false,
		},
		{
			name:    "invalid",
			level:   "invalid",
			want:    LevelInfo,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseLevel(tt.level)
			if (err != nil) != tt.wantErr {
				t.Errorf("Logger.ParseLevel() error = %v, wantErr %v", err, tt.wantErr)

				return
			}

			if got != tt.want {
				t.Errorf("Logger.ParseLevel() = %v, want %v", got, tt.want)
			}
		})
	}
}
