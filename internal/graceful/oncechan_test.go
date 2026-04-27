package graceful

import "testing"

func TestOnceChan(_ *testing.T) {
	ch := newOnceChan[int]()
	ch.Close()
	ch.Close()
	<-ch.Done()
}
