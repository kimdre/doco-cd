package notification

import (
	"errors"
	"testing"
	"time"
)

func newFailureTestNotifier(t *testing.T, interval time.Duration) *Notifier {
	t.Helper()

	notifier, err := New(Config{FailureRepeatInterval: interval})
	if err != nil {
		t.Fatal(err)
	}

	return notifier
}

func TestShouldSendFailure_SuppressesUnchangedRepeat(t *testing.T) {
	t.Parallel()

	notifier := newFailureTestNotifier(t, time.Hour)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Target: "prod", Stack: "app"})
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	if !notifier.shouldSendFailure(key, fingerprint, now) {
		t.Fatal("first failure must be sent")
	}

	if notifier.shouldSendFailure(key, fingerprint, now.Add(30*time.Second)) {
		t.Error("same failure 30s later must be suppressed")
	}

	if notifier.shouldSendFailure(key, fingerprint, now.Add(59*time.Minute)) {
		t.Error("same failure inside the repeat interval must be suppressed")
	}
}

func TestShouldSendFailure_RemindsAfterInterval(t *testing.T) {
	t.Parallel()

	notifier := newFailureTestNotifier(t, time.Hour)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Stack: "app"})
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	notifier.shouldSendFailure(key, fingerprint, now)

	if !notifier.shouldSendFailure(key, fingerprint, now.Add(time.Hour)) {
		t.Fatal("failure must be sent again once the repeat interval passed")
	}

	if notifier.shouldSendFailure(key, fingerprint, now.Add(time.Hour+time.Second)) {
		t.Error("reminder must restart the interval")
	}
}

func TestShouldSendFailure_DifferentFaultOrStackIsSent(t *testing.T) {
	t.Parallel()

	notifier := newFailureTestNotifier(t, time.Hour)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Stack: "app"})

	notifier.shouldSendFailure(key, failureFingerprint("Deployment Failed", "pull access denied"), now)

	if !notifier.shouldSendFailure(key, failureFingerprint("Deployment Failed", "hook exited with status 1"), now) {
		t.Error("a different error on the same stack must be sent")
	}

	otherStack := failureKey(Metadata{Repository: "acme/deploy", Stack: "db"})
	if !notifier.shouldSendFailure(otherStack, failureFingerprint("Deployment Failed", "pull access denied"), now) {
		t.Error("the same error on another stack must be sent")
	}
}

func TestClearFailure_LetsNextFailureThrough(t *testing.T) {
	t.Parallel()

	notifier := newFailureTestNotifier(t, time.Hour)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Stack: "app"})
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	notifier.shouldSendFailure(key, fingerprint, now)
	notifier.clearFailure(key)

	if !notifier.shouldSendFailure(key, fingerprint, now.Add(time.Second)) {
		t.Error("after a success the next failure must be sent again")
	}
}

func TestShouldSendFailure_PrunesStaleRecords(t *testing.T) {
	t.Parallel()

	notifier := newFailureTestNotifier(t, time.Hour)

	now := time.Now()
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	gone := failureKey(Metadata{Repository: "acme/deploy", Stack: "removed"})
	notifier.shouldSendFailure(gone, fingerprint, now)

	alive := failureKey(Metadata{Repository: "acme/deploy", Stack: "app"})
	notifier.shouldSendFailure(alive, fingerprint, now.Add(2*time.Hour))

	notifier.failureMu.Lock()
	_, found := notifier.lastFailures[gone]
	size := len(notifier.lastFailures)
	notifier.failureMu.Unlock()

	if found {
		t.Error("a record older than the repeat interval must be dropped")
	}

	if size != 1 {
		t.Errorf("only the current failure must be kept, got %d records", size)
	}
}

func TestShouldSendFailure_DisabledByZeroInterval(t *testing.T) {
	t.Parallel()

	notifier := newFailureTestNotifier(t, 0)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Stack: "app"})
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	for i := range 3 {
		if !notifier.shouldSendFailure(key, fingerprint, now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("suppression is off, send %d must go through", i+1)
		}
	}
}

func TestWasNotified(t *testing.T) {
	base := errors.New("hook exited with status 1")

	if WasNotified(nil) {
		t.Error("nil error was not notified")
	}

	if WasNotified(base) {
		t.Error("plain error was not notified")
	}

	marked := MarkNotified(base)

	if !WasNotified(marked) {
		t.Error("marked error must report as notified")
	}

	if !errors.Is(marked, base) {
		t.Error("marking must keep the original error unwrappable")
	}

	if marked.Error() != base.Error() {
		t.Errorf("marking must not change the message, got %q", marked.Error())
	}

	if MarkNotified(nil) != nil {
		t.Error("marking nil must stay nil")
	}
}
