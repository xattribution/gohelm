package cdp

import (
	"sync"
	"time"
)

const defaultCloseWait = 3 * time.Second

// queue runs functions one at a time, in order, without ever blocking the
// one who pushes: events keep their order and a slow subscriber can't stall
// the connection's reader.
type queue struct {
	mu   sync.Mutex
	cond *sync.Cond
	fns  []func()
	done bool
}

func newQueue() *queue {
	q := &queue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *queue) push(fn func()) {
	q.mu.Lock()
	if !q.done {
		q.fns = append(q.fns, fn)
		q.cond.Signal()
	}
	q.mu.Unlock()
}

func (q *queue) stop() {
	q.mu.Lock()
	q.done = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *queue) run() {
	for {
		q.mu.Lock()
		for len(q.fns) == 0 && !q.done {
			q.cond.Wait()
		}
		if len(q.fns) == 0 && q.done {
			q.mu.Unlock()
			return
		}
		fn := q.fns[0]
		q.fns[0] = nil
		q.fns = q.fns[1:]
		q.mu.Unlock()
		func() {
			defer func() { _ = recover() }() // one bad subscriber doesn't end delivery
			fn()
		}()
	}
}

// Queue is the same ordered runner for transports outside this package
// (the WebView2 pane delivers its events through one).
type Queue struct{ q *queue }

// NewQueue starts a queue. Stop it when done.
func NewQueue() *Queue {
	q := &Queue{q: newQueue()}
	go q.q.run()
	return q
}

// Push adds fn to run after everything pushed before it.
func (q *Queue) Push(fn func()) { q.q.push(fn) }

// Stop drops what's waiting once it has run and ends the runner.
func (q *Queue) Stop() { q.q.stop() }
