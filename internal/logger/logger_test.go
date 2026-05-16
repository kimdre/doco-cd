package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"
)

type durationLogSample struct {
	Interval time.Duration `json:"interval" yaml:"interval"`
}

type zeroValueLogSample struct {
	EmptyString string            `json:"empty_string" yaml:"empty_string"`
	ZeroInt     int               `json:"zero_int" yaml:"zero_int"`
	FalseBool   bool              `json:"false_bool" yaml:"false_bool"`
	EmptySlice  []string          `json:"empty_slice" yaml:"empty_slice"`
	EmptyMap    map[string]string `json:"empty_map" yaml:"empty_map"`
}

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

func TestBuildLogValue_FormatsDuration(t *testing.T) {
	t.Parallel()

	value := BuildLogValue(durationLogSample{Interval: 10 * time.Second})
	if got := value.Any().(map[string]any)["interval"]; got != "10s" {
		t.Fatalf("expected interval to be formatted as 10s, got %v", got)
	}
}

func TestBuildLogValue_SkipsEmptyStrings_ButKeepsOtherZeroValues(t *testing.T) {
	t.Parallel()

	value := BuildLogValue(zeroValueLogSample{
		EmptyString: "",
		ZeroInt:     0,
		FalseBool:   false,
		EmptySlice:  []string{},
		EmptyMap:    map[string]string{},
	})

	entry := value.Any().(map[string]any)

	if _, exists := entry["empty_string"]; exists {
		t.Fatal("expected empty_string to be omitted")
	}

	if got := entry["zero_int"]; got != 0 {
		t.Fatalf("expected zero_int to be preserved as 0, got %v", got)
	}

	if got := entry["false_bool"]; got != false {
		t.Fatalf("expected false_bool to be preserved as false, got %v", got)
	}

	if got, ok := entry["empty_slice"]; !ok {
		t.Fatal("expected empty_slice to be preserved")
	} else if slice, ok := got.([]any); !ok || len(slice) != 0 {
		t.Fatalf("expected empty_slice to be an empty slice, got %T %v", got, got)
	}

	if got, ok := entry["empty_map"]; !ok {
		t.Fatal("expected empty_map to be preserved")
	} else if m, ok := got.(map[string]any); !ok || len(m) != 0 {
		t.Fatalf("expected empty_map to be an empty map, got %T %v", got, got)
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
