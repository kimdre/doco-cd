package graceful

import "testing"

func TestOnceChan(t *testing.T) {
	t.Parallel()

	ch := newOnceChan[int]()
	ch.Close()
	ch.Close()
	<-ch.Done()
}
