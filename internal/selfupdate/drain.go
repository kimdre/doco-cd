package selfupdate

// drainRequests is buffered so a deploy goroutine never blocks on the
// coordinator, and a second request while one is pending is a no-op.
var drainRequests = make(chan struct{}, 1)

// RequestDrain asks the coordinator to stop accepting work and hand over.
func RequestDrain() {
	select {
	case drainRequests <- struct{}{}:
	default:
	}
}

// DrainRequests is the coordinator's side of [RequestDrain].
func DrainRequests() <-chan struct{} {
	return drainRequests
}
