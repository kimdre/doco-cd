package notification

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// A failure notification repeats as often as the trigger does. A poll job runs
// every interval and a broken deployment fails identically every run, so a
// single fault turns into a stream of identical messages until someone fixes it
// - an expired registry token produced around 240 of them in two hours. Track
// the last failure per stack, stay quiet while it is unchanged, and remind once
// per failureRepeatInterval so a long outage is not forgotten instead. A
// successful notification for the same stack clears the entry, which makes the
// existing "Deployment completed" notification the recovery signal.

// DefaultFailureRepeatInterval is how long an unchanged failure stays quiet
// before it is sent again as a reminder.
const DefaultFailureRepeatInterval = time.Hour

var (
	failureMu             sync.Mutex
	lastFailures          = map[string]failureRecord{}
	failureRepeatInterval = DefaultFailureRepeatInterval
)

// failureRecord is the last failure notification sent for one stack.
type failureRecord struct {
	fingerprint string
	sentAt      time.Time
}

// SetFailureRepeatInterval sets how long an unchanged failure is suppressed.
// Zero or less turns suppression off entirely: every failure is sent, as it was
// before this existed.
func SetFailureRepeatInterval(interval time.Duration) {
	failureMu.Lock()
	defer failureMu.Unlock()

	failureRepeatInterval = interval

	if interval <= 0 {
		lastFailures = map[string]failureRecord{}
	}
}

// failureKey identifies the thing that failed. The stack is what an operator
// acts on, and the rest keeps two stacks of the same name apart when one daemon
// serves several repositories, targets or docker contexts.
func failureKey(m Metadata) string {
	return strings.Join([]string{m.Repository, m.Target, m.Stack, m.Context}, "|")
}

// failureFingerprint identifies the fault itself. Title and message only: job
// id, duration and revision differ on every attempt, so including them would
// make every repeat look new.
func failureFingerprint(title, message string) string {
	sum := sha256.Sum256([]byte(title + "\n" + message))

	return hex.EncodeToString(sum[:])
}

// shouldSendFailure reports whether this failure is worth sending: it is new,
// it differs from the last one for the same stack, or the reminder interval has
// passed. It records what it lets through.
func shouldSendFailure(key, fingerprint string, now time.Time) bool {
	failureMu.Lock()
	defer failureMu.Unlock()

	if failureRepeatInterval <= 0 {
		return true
	}

	pruneFailures(now)

	last, found := lastFailures[key]
	if found && last.fingerprint == fingerprint && now.Sub(last.sentAt) < failureRepeatInterval {
		return false
	}

	lastFailures[key] = failureRecord{fingerprint: fingerprint, sentAt: now}

	return true
}

// pruneFailures drops records that cannot suppress anything anymore. Past the
// repeat interval the next failure is sent whatever the record says, so keeping
// it only grows the map for stacks that are renamed, removed or fixed without a
// success notification. Caller holds failureMu.
func pruneFailures(now time.Time) {
	for key, record := range lastFailures {
		if now.Sub(record.sentAt) >= failureRepeatInterval {
			delete(lastFailures, key)
		}
	}
}

// clearFailure forgets the last failure of a stack, so the next one is sent
// immediately instead of being taken for a repeat.
func clearFailure(key string) {
	failureMu.Lock()
	defer failureMu.Unlock()

	delete(lastFailures, key)
}
