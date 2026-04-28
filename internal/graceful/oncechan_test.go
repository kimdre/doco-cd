package graceful

import "testing"

func TestOnceChan(t *testing.T) {
	t.Parallel()

	ch := newOnceChan[int]()
	// close twice should not panic
	ch.Close()
	ch.Close()
	<-ch.Done()
}
