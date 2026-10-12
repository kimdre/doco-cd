package scheduler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
)

func TestExecutionStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := newExecutionStore(t.TempDir())
	record := executionRecord{
		RunID:        "run-1",
		Context:      "production",
		Mode:         scheduledJobModeContainer,
		Job:          executionJob{Key: "container:job", Name: "job", ID: "source", Mode: scheduledJobModeContainer},
		Finalization: executionFinalizationConfig{NotifyOn: docker.JobNotifyAll},
		Restored:     false,
	}

	if err := store.create(&record); err != nil {
		t.Fatalf("create: %v", err)
	}

	record.Reported = true
	if err := store.update(record); err != nil {
		t.Fatalf("update: %v", err)
	}

	records, err := store.list("production", scheduledJobModeContainer)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(records) != 1 || !records[0].Reported {
		t.Fatalf("records = %#v, want updated record", records)
	}

	current, err := store.get(record.RunID)
	if err != nil || current == nil || !current.Reported {
		t.Fatalf("get = %#v, %v, want updated record", current, err)
	}

	if err := store.remove(record.RunID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	records, err = store.list("production", scheduledJobModeContainer)
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}

	if len(records) != 0 {
		t.Fatalf("records after remove = %#v, want none", records)
	}

	current, err = store.get(record.RunID)
	if err != nil || current != nil {
		t.Fatalf("get after remove = %#v, %v, want no record", current, err)
	}
}

func TestExecutionStoreGetRejectsInvalidRecords(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "malformed", content: "{not json"},
		{name: "unsupported version", content: `{"version":999,"run_id":"run"}`},
		{name: "mismatched identity", content: `{"version":1,"run_id":"other"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := newExecutionStore(t.TempDir())

			record := executionRecord{RunID: "run"}
			if err := store.create(&record); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(store.path(record.RunID), []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}

			if current, err := store.get(record.RunID); err == nil || current != nil {
				t.Fatalf("invalid record accepted: %#v, %v", current, err)
			}
		})
	}
}

func TestExecutionStoreGetSurfacesReadErrors(t *testing.T) {
	t.Parallel()

	store := newExecutionStore(t.TempDir())
	if err := os.MkdirAll(store.path("run"), 0o755); err != nil {
		t.Fatal(err)
	}

	if record, err := store.get("run"); err == nil || record != nil {
		t.Fatalf("read error not surfaced: %#v, %v", record, err)
	}
}

func TestExecutionStoreGetDisabled(t *testing.T) {
	t.Parallel()

	if record, err := newExecutionStore("").get("run"); err != nil || record != nil {
		t.Fatalf("disabled lookup = %#v, %v", record, err)
	}
}

func TestExecutionStoreQuarantinesMalformedRecords(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	store := newExecutionStore(root)
	if err := os.MkdirAll(store.root, 0o755); err != nil {
		t.Fatalf("create store: %v", err)
	}

	badPath := filepath.Join(store.root, "bad.json")
	if err := os.WriteFile(badPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed record: %v", err)
	}

	records, err := store.list("", scheduledJobModeContainer)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(records) != 0 {
		t.Fatalf("records = %#v, want none", records)
	}

	if _, err := os.Stat(badPath + ".corrupt"); err != nil {
		t.Fatalf("malformed record was not quarantined: %v", err)
	}
}
