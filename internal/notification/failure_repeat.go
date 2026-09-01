package notification

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
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

// failureRecord is the last failure notification sent for one stack.
type failureRecord struct {
	fingerprint string
	sentAt      time.Time
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
func (n *Notifier) shouldSendFailure(key, fingerprint string, now time.Time) bool {
	n.failureMu.Lock()
	defer n.failureMu.Unlock()

	if n.failureRepeatInterval <= 0 {
		return true
	}

	n.pruneFailures(now)

	last, found := n.lastFailures[key]
	if found && last.fingerprint == fingerprint && now.Sub(last.sentAt) < n.failureRepeatInterval {
		return false
	}

	n.lastFailures[key] = failureRecord{fingerprint: fingerprint, sentAt: now}

	return true
}

// pruneFailures drops records that cannot suppress anything anymore. Past the
// repeat interval the next failure is sent whatever the record says, so keeping
// it only grows the map for stacks that are renamed, removed or fixed without a
// success notification. Caller holds n.failureMu.
func (n *Notifier) pruneFailures(now time.Time) {
	for key, record := range n.lastFailures {
		if now.Sub(record.sentAt) >= n.failureRepeatInterval {
			delete(n.lastFailures, key)
		}
	}
}

// clearFailure forgets the last failure of a stack, so the next one is sent
// immediately instead of being taken for a repeat.
func (n *Notifier) clearFailure(key string) {
	n.failureMu.Lock()
	defer n.failureMu.Unlock()

	delete(n.lastFailures, key)
}

// clearUnsentFailure removes this attempt's suppression record after delivery
// failed, without deleting a newer failure recorded concurrently for the stack.
func (n *Notifier) clearUnsentFailure(key, fingerprint string, sentAt time.Time) {
	n.failureMu.Lock()
	defer n.failureMu.Unlock()

	record, found := n.lastFailures[key]
	if found && record.fingerprint == fingerprint && record.sentAt.Equal(sentAt) {
		delete(n.lastFailures, key)
	}
}
