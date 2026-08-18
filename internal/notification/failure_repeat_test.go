package notification

import (
	"errors"
	"testing"
	"time"
)

// resetFailureState clears the suppression state so tests do not leak into each
// other. They cannot run in parallel: the state is package level, like the
// apprise config the other tests here mutate.
func resetFailureState(t *testing.T, interval time.Duration) {
	t.Helper()

	failureMu.Lock()
	lastFailures = map[string]failureRecord{}
	failureRepeatInterval = interval
	failureMu.Unlock()

	t.Cleanup(func() {
		failureMu.Lock()
		lastFailures = map[string]failureRecord{}
		failureRepeatInterval = DefaultFailureRepeatInterval
		failureMu.Unlock()
	})
}

func TestShouldSendFailure_SuppressesUnchangedRepeat(t *testing.T) {
	resetFailureState(t, time.Hour)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Target: "prod", Stack: "app"})
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	if !shouldSendFailure(key, fingerprint, now) {
		t.Fatal("first failure must be sent")
	}

	if shouldSendFailure(key, fingerprint, now.Add(30*time.Second)) {
		t.Error("same failure 30s later must be suppressed")
	}

	if shouldSendFailure(key, fingerprint, now.Add(59*time.Minute)) {
		t.Error("same failure inside the repeat interval must be suppressed")
	}
}

func TestShouldSendFailure_RemindsAfterInterval(t *testing.T) {
	resetFailureState(t, time.Hour)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Stack: "app"})
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	shouldSendFailure(key, fingerprint, now)

	if !shouldSendFailure(key, fingerprint, now.Add(time.Hour)) {
		t.Fatal("failure must be sent again once the repeat interval passed")
	}

	if shouldSendFailure(key, fingerprint, now.Add(time.Hour+time.Second)) {
		t.Error("reminder must restart the interval")
	}
}

func TestShouldSendFailure_DifferentFaultOrStackIsSent(t *testing.T) {
	resetFailureState(t, time.Hour)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Stack: "app"})

	shouldSendFailure(key, failureFingerprint("Deployment Failed", "pull access denied"), now)

	if !shouldSendFailure(key, failureFingerprint("Deployment Failed", "hook exited with status 1"), now) {
		t.Error("a different error on the same stack must be sent")
	}

	otherStack := failureKey(Metadata{Repository: "acme/deploy", Stack: "db"})
	if !shouldSendFailure(otherStack, failureFingerprint("Deployment Failed", "pull access denied"), now) {
		t.Error("the same error on another stack must be sent")
	}
}

func TestClearFailure_LetsNextFailureThrough(t *testing.T) {
	resetFailureState(t, time.Hour)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Stack: "app"})
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	shouldSendFailure(key, fingerprint, now)
	clearFailure(key)

	if !shouldSendFailure(key, fingerprint, now.Add(time.Second)) {
		t.Error("after a success the next failure must be sent again")
	}
}

func TestShouldSendFailure_DisabledByZeroInterval(t *testing.T) {
	resetFailureState(t, 0)

	now := time.Now()
	key := failureKey(Metadata{Repository: "acme/deploy", Stack: "app"})
	fingerprint := failureFingerprint("Deployment Failed", "pull access denied")

	for i := range 3 {
		if !shouldSendFailure(key, fingerprint, now.Add(time.Duration(i)*time.Second)) {
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
