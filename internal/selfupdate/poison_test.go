package selfupdate

import "testing"

func TestIsPoisoned(t *testing.T) {
	t.Parallel()

	base := Poison{
		Context:     "default",
		Stack:       "doco-cd",
		CommitSHA:   "aaa",
		ProjectHash: "hash1",
		Reason:      "successor never became healthy",
	}

	tests := []struct {
		name                     string
		ctx, stack, commit, hash string
		want                     bool
	}{
		{name: "same everything", ctx: "default", stack: "doco-cd", commit: "aaa", hash: "hash1", want: true},
		{name: "different commit", ctx: "default", stack: "doco-cd", commit: "bbb", hash: "hash1"},
		{name: "different hash", ctx: "default", stack: "doco-cd", commit: "aaa", hash: "hash2"},
		{name: "different stack", ctx: "default", stack: "other", commit: "aaa", hash: "hash1"},
		{name: "different context", ctx: "remote", stack: "doco-cd", commit: "aaa", hash: "hash1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := newTestStore(t)
			if err := store.AddPoison(base); err != nil {
				t.Fatalf("add poison: %v", err)
			}

			_, got, err := store.IsPoisoned(tt.ctx, tt.stack, tt.commit, tt.hash)
			if err != nil {
				t.Fatalf("is poisoned: %v", err)
			}

			if got != tt.want {
				t.Errorf("poisoned = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAddPoisonCountsAttempts(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	p := Poison{Context: "default", Stack: "doco-cd", CommitSHA: "aaa", ProjectHash: "hash1"}

	for range 2 {
		if err := store.AddPoison(p); err != nil {
			t.Fatalf("add poison: %v", err)
		}
	}

	got, ok, err := store.IsPoisoned("default", "doco-cd", "aaa", "hash1")
	if err != nil || !ok {
		t.Fatalf("is poisoned: ok=%v err=%v", ok, err)
	}

	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.Attempts)
	}

	// A new commit for the same stack replaces the entry and resets the count.
	p.CommitSHA = "bbb"
	if err = store.AddPoison(p); err != nil {
		t.Fatalf("add poison for new commit: %v", err)
	}

	got, ok, err = store.IsPoisoned("default", "doco-cd", "bbb", "hash1")
	if err != nil || !ok {
		t.Fatalf("is poisoned after new commit: ok=%v err=%v", ok, err)
	}

	if got.Attempts != 1 {
		t.Errorf("attempts after new commit = %d, want 1", got.Attempts)
	}
}

func TestClearPoison(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	if err := store.ClearPoison("default", "doco-cd"); err != nil {
		t.Fatalf("clear on empty store: %v", err)
	}

	if err := store.AddPoison(Poison{Context: "default", Stack: "doco-cd", CommitSHA: "aaa", ProjectHash: "hash1"}); err != nil {
		t.Fatalf("add poison: %v", err)
	}

	if err := store.ClearPoison("default", "doco-cd"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	_, ok, err := store.IsPoisoned("default", "doco-cd", "aaa", "hash1")
	if err != nil {
		t.Fatalf("is poisoned: %v", err)
	}

	if ok {
		t.Error("poison survived ClearPoison")
	}
}

func TestPoisonFileIsNotAnActiveRecord(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	if err := store.AddPoison(Poison{Context: "default", Stack: "doco-cd", CommitSHA: "aaa"}); err != nil {
		t.Fatalf("add poison: %v", err)
	}

	active, err := store.Active()
	if err != nil {
		t.Fatalf("active: %v", err)
	}

	if active != nil {
		t.Errorf("poison.json was read as an active record: %v", active)
	}
}
