package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestLogRecoveredPanic(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	log := slog.New(slog.NewJSONHandler(&output, nil))

	LogRecoveredPanic(log, "test work", "panic value")

	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}

	if entry["msg"] != "recovered from panic in background task" ||
		entry["context"] != "test work" ||
		entry["panic"] != "panic value" {
		t.Fatalf("unexpected panic log entry: %#v", entry)
	}

	stack, ok := entry["stack"].(string)
	if !ok || !strings.Contains(stack, "TestLogRecoveredPanic") {
		t.Fatalf("panic stack missing caller: %#v", entry["stack"])
	}
}
