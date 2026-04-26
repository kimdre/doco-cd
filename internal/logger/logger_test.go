package logger

import (
	"bytes"
	"encoding/json"
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

func TestWithoutAttr_RemovesAttachedAttribute(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	base := slog.New(newAttrFilterHandler(slog.NewJSONHandler(&buf, nil)))
	log := base.With(slog.String("job_id", "123"), slog.String("repository", "repo"))

	WithoutAttr(log, "job_id").Info("hello")

	entry := decodeJSONLogLine(t, buf.Bytes())
	if _, exists := entry["job_id"]; exists {
		t.Fatalf("expected job_id to be removed, got %v", entry)
	}

	if got := entry["repository"]; got != "repo" {
		t.Fatalf("expected repository attr to be preserved, got %v", got)
	}

	if got := entry["msg"]; got != "hello" {
		t.Fatalf("expected msg to be hello, got %v", got)
	}
}

func TestWithoutAttr_MissingAttributeKeepsLoggerOutput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	base := slog.New(newAttrFilterHandler(slog.NewJSONHandler(&buf, nil)))
	log := base.With(slog.String("repository", "repo"))

	WithoutAttr(log, "job_id").Info("hello", slog.String("stack", "app"))

	entry := decodeJSONLogLine(t, buf.Bytes())
	if got := entry["repository"]; got != "repo" {
		t.Fatalf("expected repository attr to be preserved, got %v", got)
	}

	if got := entry["stack"]; got != "app" {
		t.Fatalf("expected stack attr to be preserved, got %v", got)
	}
}

func decodeJSONLogLine(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &entry); err != nil {
		t.Fatalf("failed to decode log line %q: %v", string(raw), err)
	}

	return entry
}
