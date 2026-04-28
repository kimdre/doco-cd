package graceful

import (
	"sync"
)

type onceChan[T any] struct {
	ch   chan T
	once sync.Once
}

func newOnceChan[T any]() *onceChan[T] {
	return &onceChan[T]{
		ch: make(chan T),
	}
}

func (o *onceChan[T]) Close() {
	o.once.Do(func() {
		close(o.ch)
	})
}

func (o *onceChan[T]) Done() <-chan T {
	return o.ch
}
