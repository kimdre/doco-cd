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
