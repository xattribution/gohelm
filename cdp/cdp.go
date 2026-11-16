// Package cdp is a small Chrome DevTools Protocol client.
//
// Everything in gohelm that drives a page talks to a Transport, so the same
// driver works over a WebView2 window's own DevTools API (package pane), a
// pipe to a browser gohelm started (package pipeconn), or a WebSocket
// (package wsconn).
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Transport sends DevTools commands to one target (a page) and delivers its
// events.
type Transport interface {
	// Call sends a command and waits for its result. params may be nil.
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	// Subscribe calls fn for each event with this name until cancel is
	// called. fn runs on a goroutine that delivers events in order; it may
	// call Call, but it must not block for long.
	Subscribe(event string, fn func(params json.RawMessage)) (cancel func())
	// Close ends the transport. Calls in flight return ErrClosed.
	Close() error
}

// ErrClosed is returned by calls on a transport that has closed.
var ErrClosed = errors.New("cdp: connection closed")

// Error is an error the browser returned for a command.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("cdp: %s (%d): %s", e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("cdp: %s (%d)", e.Message, e.Code)
}

// MessageConn is a connection carrying whole JSON messages: a WebSocket or
// a pipe.
type MessageConn interface {
	ReadMessage() ([]byte, error)
	WriteMessage([]byte) error
	Close() error
}

type message struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *Error          `json:"error,omitempty"`
}

type outgoing struct {
	ID        int64  `json:"id"`
	SessionID string `json:"sessionId,omitempty"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
}

// Conn multiplexes one browser connection: commands to the browser and to
// any number of attached sessions, and their events.
type Conn struct {
	mc     MessageConn
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan message
	subs    map[subKey]map[int64]func(json.RawMessage)
	subID   int64
	closed  bool
	err     error

	wmu  sync.Mutex // one writer at a time
	done chan struct{}
	evq  *queue
}

type subKey struct{ session, event string }

// NewConn starts reading mc. Close the Conn to close mc.
func NewConn(mc MessageConn) *Conn {
	c := &Conn{
		mc:      mc,
		pending: map[int64]chan message{},
		subs:    map[subKey]map[int64]func(json.RawMessage){},
		done:    make(chan struct{}),
		evq:     newQueue(),
	}
	go c.readLoop()
	go c.evq.run()
	return c
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended, once Done is closed.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) readLoop() {
	var err error
	for {
		var b []byte
		b, err = c.mc.ReadMessage()
		if err != nil {
			break
		}
		var m message
		if json.Unmarshal(b, &m) != nil {
			continue // not ours to fail on
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		if m.Method == "" {
			continue
		}
		c.mu.Lock()
		var fns []func(json.RawMessage)
		for _, fn := range c.subs[subKey{m.SessionID, m.Method}] {
			fns = append(fns, fn)
		}
		c.mu.Unlock()
		params := m.Params
		for _, fn := range fns {
			c.evq.push(func() { fn(params) })
		}
	}
	c.shutdown(err)
}

func (c *Conn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if err == nil {
		err = ErrClosed
	}
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.mc.Close()
	c.evq.stop()
	close(c.done)
}

// Close ends the connection.
func (c *Conn) Close() error {
	c.shutdown(ErrClosed)
	return nil
}

func (c *Conn) call(ctx context.Context, session, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()
	b, err := json.Marshal(outgoing{ID: id, SessionID: session, Method: method, Params: params})
	if err != nil {
		c.forget(id)
		return nil, err
	}
	c.wmu.Lock()
	err = c.mc.WriteMessage(b)
	c.wmu.Unlock()
	if err != nil {
		c.forget(id)
		return nil, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, ErrClosed
		}
		if m.Error != nil {
			return nil, m.Error
		}
		if len(m.Result) == 0 {
			return json.RawMessage("{}"), nil
		}
		return m.Result, nil
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	}
}

func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Conn) subscribe(session, event string, fn func(json.RawMessage)) func() {
	c.mu.Lock()
	c.subID++
	id := c.subID
	k := subKey{session, event}
	if c.subs[k] == nil {
		c.subs[k] = map[int64]func(json.RawMessage){}
	}
	c.subs[k][id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs[k], id)
		if len(c.subs[k]) == 0 {
			delete(c.subs, k)
		}
		c.mu.Unlock()
	}
}

// Call sends a command to the browser itself (no session).
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.call(ctx, "", method, params)
}

// Subscribe delivers the browser's own events (no session).
func (c *Conn) Subscribe(event string, fn func(json.RawMessage)) func() {
	return c.subscribe("", event, fn)
}

// Session is a Transport for one attached target.
func (c *Conn) Session(id string) Transport { return &session{c: c, id: id} }

type session struct {
	c  *Conn
	id string
}

func (s *session) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return s.c.call(ctx, s.id, method, params)
}

func (s *session) Subscribe(event string, fn func(json.RawMessage)) func() {
	return s.c.subscribe(s.id, event, fn)
}

// Close detaches the session; the browser connection stays open.
func (s *session) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCloseWait)
	defer cancel()
	_, err := s.c.Call(ctx, "Target.detachFromTarget", map[string]any{"sessionId": s.id})
	return err
}

// Do calls method and decodes its result into out (which may be nil).
func Do(ctx context.Context, t Transport, method string, params, out any) error {
	raw, err := t.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("cdp: decoding %s: %w", method, err)
	}
	return nil
}
