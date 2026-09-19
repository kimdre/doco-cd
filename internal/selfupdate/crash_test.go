package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaybeCrashOnlyFiresOnceForTheRequestedPoint(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	calls := 0
	exit := func(int) { calls++ }

	maybeCrash(data, "handover", nil, "", exit)

	if calls != 0 {
		t.Fatalf("crashed with no crash point configured")
	}

	maybeCrash(data, "handover", nil, "applied", exit)

	if calls != 0 {
		t.Fatalf("crashed at the wrong point")
	}

	maybeCrash(data, "handover", nil, "handover", exit)

	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	if _, err := os.Stat(filepath.Join(data, "self-update", "crash-handover")); err != nil {
		t.Fatalf("marker was not written: %v", err)
	}

	maybeCrash(data, "handover", nil, "handover", exit)

	if calls != 1 {
		t.Errorf("crash fired a second time, calls = %d", calls)
	}
}
