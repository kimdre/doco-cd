package selfupdate

// InApplierPhase reports states in which an applier still owns the handover
// and can recover it.
func (s State) InApplierPhase() bool {
	return s == StateApplying || s == StateApplyReady || s == StateApplyDrained
}

// ApplierFinished reports states the applier must not apply or recover again.
func (s State) ApplierFinished() bool {
	return s == StateFailed || s == StateRolledBack || s == StateApplied || s == StateFinalising
}

// RolledBackOrFailed reports the applier's unsuccessful outcomes.
func (s State) RolledBackOrFailed() bool {
	return s == StateFailed || s == StateRolledBack
}

// Unsuccessful reports outcomes the predecessor reports and cleans up.
func (s State) Unsuccessful() bool {
	return s.RolledBackOrFailed() || s == StateAborted
}
