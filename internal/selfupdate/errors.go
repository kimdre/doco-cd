package selfupdate

import "errors"

var (
	// ErrHandover signals that a deploy handed the stack over to another
	// process. It is neither a success nor a failure for the calling stage.
	ErrHandover = errors.New("self-update handover in progress")

	// ErrDisabled is returned when a stack contains this doco-cd instance but
	// the self-update feature is off.
	ErrDisabled = errors.New("self-update is disabled")

	// ErrUnsupported is returned when the self stack cannot be updated by any
	// strategy.
	ErrUnsupported = errors.New("self-update is not supported for this stack")

	// ErrInvalidTransition is returned when a journal state change is not allowed.
	ErrInvalidTransition = errors.New("invalid self-update state transition")

	// ErrStaleRecord is returned when another process advanced or removed a
	// journal record before a Save or Update could persist its changes.
	ErrStaleRecord = errors.New("stale self-update record")

	// ErrNoRecord is returned when no journal record exists for an ID.
	ErrNoRecord = errors.New("no self-update record")
)
