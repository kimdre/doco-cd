package docker

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/test"
)

type countingLocker struct {
	mu sync.Mutex

	holdDelay time.Duration

	lockCalls   atomic.Int64
	unlockCalls atomic.Int64

	held      bool
	violation atomic.Bool
}

func (l *countingLocker) Lock() {
	l.mu.Lock()
	l.lockCalls.Add(1)

	if l.held {
		l.violation.Store(true)
	}

	l.held = true

	if l.holdDelay > 0 {
		time.Sleep(l.holdDelay)
	}
}

func (l *countingLocker) Unlock() {
	l.held = false
	l.unlockCalls.Add(1)
	l.mu.Unlock()
}

func newTestComposeProject(t *testing.T, name string) (workingDir string, composeFile string, stackName string) {
	t.Helper()

	workingDir = t.TempDir()
	composeFile = filepath.Join(workingDir, "test.compose.yaml")

	createComposeFile(t, composeFile, generateComposeContents())

	return workingDir, composeFile, test.ConvertTestName(name)
}

func TestLoadCompose_MutationLockSerializesMutationPhases(t *testing.T) {
	t.Parallel()

	const goroutines = 6

	sharedLock := &countingLocker{holdDelay: 5 * time.Millisecond}

	var wg sync.WaitGroup

	errs := make([]error, goroutines)

	for i := range goroutines {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			workingDir, composeFile, stackName := newTestComposeProject(t, t.Name()+"-"+strconv.Itoa(i))

			_, err := LoadCompose(
				context.Background(), nil, workingDir, workingDir, stackName,
				[]string{composeFile}, []string{".env"}, []string{}, map[string]string{},
				ComposeLoadOptions{MutationLock: sharedLock},
			)
			errs[i] = err
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: LoadCompose() error = %v", i, err)
		}
	}

	if sharedLock.violation.Load() {
		t.Fatal("MutationLock observed two holders inside a locked section at the same time")
	}

	wantCycles := int64(goroutines * 2)
	if got := sharedLock.lockCalls.Load(); got != wantCycles {
		t.Fatalf("Lock() call count = %d, want %d (2 mutation phases per call)", got, wantCycles)
	}

	if got := sharedLock.unlockCalls.Load(); got != wantCycles {
		t.Fatalf("Unlock() call count = %d, want %d (2 mutation phases per call)", got, wantCycles)
	}
}

func TestLoadCompose_MutationLockScopedToTwoDiscretePhases(t *testing.T) {
	t.Parallel()

	workingDir, composeFile, stackName := newTestComposeProject(t, t.Name())

	l := &countingLocker{}

	project, err := LoadCompose(
		context.Background(), nil, workingDir, workingDir, stackName,
		[]string{composeFile}, []string{".env"}, []string{}, map[string]string{},
		ComposeLoadOptions{MutationLock: l},
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(project.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(project.Services))
	}

	if got := l.lockCalls.Load(); got != 2 {
		t.Fatalf("Lock() call count = %d, want 2 (decrypt-files phase + decrypt-project-files phase)", got)
	}

	if got := l.unlockCalls.Load(); got != 2 {
		t.Fatalf("Unlock() call count = %d, want 2", got)
	}

	if l.violation.Load() {
		t.Fatal("MutationLock reported overlapping holders for a single, sequential call")
	}
}

func TestLoadCompose_NilMutationLockPreservesPriorBehavior(t *testing.T) {
	t.Parallel()

	workingDir, composeFile, stackName := newTestComposeProject(t, t.Name())

	project, err := LoadCompose(
		context.Background(), nil, workingDir, workingDir, stackName,
		[]string{composeFile}, []string{".env"}, []string{}, map[string]string{},
		ComposeLoadOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(project.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(project.Services))
	}
}
