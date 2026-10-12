package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/logger"
)

type mcpLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *mcpLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(data)
}

func (b *mcpLogBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	decoder := json.NewDecoder(bytes.NewReader(b.buffer.Bytes()))

	var entries []map[string]any

	for {
		var entry map[string]any

		err := decoder.Decode(&entry)
		if errors.Is(err, io.EOF) {
			return entries
		}

		if err != nil {
			t.Fatalf("decode MCP log entry: %v", err)
		}

		entries = append(entries, entry)
	}
}

func TestSDKLogHandlerLevels(t *testing.T) {
	t.Parallel()

	levels := []slog.Level{
		slog.LevelDebug,
		slog.LevelInfo,
		slog.LevelWarn,
		slog.LevelError,
		logger.LevelCritical,
	}

	for _, minimum := range levels {
		t.Run(minimum.String(), func(t *testing.T) {
			t.Parallel()

			for _, level := range levels {
				t.Run(level.String(), func(t *testing.T) {
					t.Parallel()

					var output mcpLogBuffer

					log := slog.New(&sdkLogHandler{
						next: slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: minimum}),
					})

					wantLevel := level
					if level == slog.LevelInfo {
						wantLevel = slog.LevelDebug
					}

					wantEnabled := wantLevel >= minimum
					if got := log.Enabled(t.Context(), level); got != wantEnabled {
						t.Fatalf("Enabled(%v) = %v, want %v", level, got, wantEnabled)
					}

					log.Log(t.Context(), level, "SDK message", slog.String("request_id", "request-1"))

					entries := output.entries(t)
					if !wantEnabled {
						if len(entries) != 0 {
							t.Fatalf("disabled record was logged: %v", entries)
						}

						return
					}

					if len(entries) != 1 {
						t.Fatalf("got %d entries, want one", len(entries))
					}

					entry := entries[0]
					if entry["level"] != wantLevel.String() {
						t.Errorf("level = %v, want %v", entry["level"], wantLevel)
					}

					if entry["msg"] != "SDK message" || entry["request_id"] != "request-1" {
						t.Errorf("record fields were not preserved: %v", entry)
					}
				})
			}
		})
	}
}

func TestSDKLogHandlerWithAttrsAndGroups(t *testing.T) {
	t.Parallel()

	var output mcpLogBuffer

	base := slog.New(&sdkLogHandler{
		next: slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}),
	})
	log := base.With(slog.String("component", "mcp")).
		WithGroup("request").
		With(slog.String("request_id", "request-1")).
		WithGroup("tool").
		With(slog.String("name", "list_projects"))

	log.Info("SDK message", slog.String("status", "success"))
	base.Info("base SDK message")

	entries := output.entries(t)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want two", len(entries))
	}

	entry := entries[0]
	if entry["level"] != "DEBUG" || entry["component"] != "mcp" {
		t.Fatalf("level or component was not preserved: %v", entry)
	}

	request, ok := entry["request"].(map[string]any)
	if !ok || request["request_id"] != "request-1" {
		t.Fatalf("request attributes were not preserved: %v", entry)
	}

	tool, ok := request["tool"].(map[string]any)
	if !ok || tool["name"] != "list_projects" || tool["status"] != "success" {
		t.Fatalf("tool attributes were not preserved: %v", request)
	}

	baseEntry := entries[1]
	if baseEntry["level"] != "DEBUG" {
		t.Errorf("base logger level = %v, want DEBUG", baseEntry["level"])
	}

	if _, exists := baseEntry["component"]; exists {
		t.Errorf("derived attributes changed the base logger: %v", baseEntry)
	}

	if _, exists := baseEntry["request"]; exists {
		t.Errorf("derived groups changed the base logger: %v", baseEntry)
	}
}

type mcpRecordHandler struct {
	slog.Handler
	record slog.Record
	ctx    context.Context
	err    error
}

func (h *mcpRecordHandler) Handle(ctx context.Context, record slog.Record) error {
	h.record = record.Clone()
	h.ctx = ctx

	return h.err
}

func TestSDKLogHandlerPreservesRecordAndError(t *testing.T) {
	t.Parallel()

	next := &mcpRecordHandler{
		Handler: slog.NewJSONHandler(io.Discard, nil),
		err:     errors.New("handler failed"),
	}
	handler := &sdkLogHandler{next: next}
	record := slog.NewRecord(time.Date(2026, time.October, 12, 16, 22, 0, 0, time.UTC), slog.LevelInfo, "SDK message", 123)
	record.AddAttrs(slog.String("session_id", "session-1"))

	if err := handler.Handle(t.Context(), record); !errors.Is(err, next.err) {
		t.Fatalf("Handle() error = %v, want %v", err, next.err)
	}

	if got := next.record; got.Level != slog.LevelDebug || got.Time != record.Time || got.Message != record.Message || got.PC != record.PC {
		t.Errorf("record metadata was not preserved: %#v", got)
	}

	if next.ctx != t.Context() {
		t.Error("record context was not preserved")
	}

	var attrs []slog.Attr

	next.record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)

		return true
	})

	if len(attrs) != 1 || !attrs[0].Equal(slog.String("session_id", "session-1")) {
		t.Errorf("record attributes were not preserved: %v", attrs)
	}

	if record.Level != slog.LevelInfo {
		t.Errorf("original record level changed to %v", record.Level)
	}
}
