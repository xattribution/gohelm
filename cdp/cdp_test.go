package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xattribution/gohelm/cdp/pipeconn"
)

// newPair connects a Conn to a fake browser that answers with handle.
func newPair(t *testing.T, handle func(m message) []any) *Conn {
	t.Helper()
	cr, bw := io.Pipe() // browser → client
	br, cw := io.Pipe() // client → browser
	go func() {
		pc := pipeconn.New(br, bw, nil)
		for {
			b, err := pc.ReadMessage()
			if err != nil {
				return
			}
			var m message
			json.Unmarshal(b, &m)
			for _, reply := range handle(m) {
				rb, _ := json.Marshal(reply)
				if pc.WriteMessage(rb) != nil {
					return
				}
			}
		}
	}()
	c := NewConn(pipeconn.New(cr, cw, func() error { cw.Close(); return cr.Close() }))
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCallsAndEvents(t *testing.T) {
	c := newPair(t, func(m message) []any {
		switch m.Method {
		case "Echo":
			return []any{
				map[string]any{"method": "Page.loadEventFired", "sessionId": m.SessionID, "params": map[string]any{"n": 1}},
				map[string]any{"id": m.ID, "sessionId": m.SessionID, "result": map[string]any{"got": m.SessionID}},
			}
		case "Fail":
			return []any{map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "nope"}}}
		}
		return nil
	})
	ctx := context.Background()
	s := c.Session("S1")
	var mu sync.Mutex
	var got []string
	cancel := s.Subscribe("Page.loadEventFired", func(p json.RawMessage) { mu.Lock(); got = append(got, string(p)); mu.Unlock() })
	var r struct{ Got string }
	if err := Do(ctx, s, "Echo", nil, &r); err != nil || r.Got != "S1" {
		t.Fatalf("echo: %v %+v", err, r)
	}
	// The browser-level subscriber doesn't get a session's events.
	c.Subscribe("Page.loadEventFired", func(json.RawMessage) { t.Error("browser-level got a session event") })
	if err := Do(ctx, s, "Echo", nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(got) != 2 || !strings.Contains(got[0], `"n":1`) {
		t.Errorf("events: %v", got)
	}
	mu.Unlock()
	cancel()
	_, err := c.Call(ctx, "Fail", nil)
	var ce *Error
	if !errors.As(err, &ce) || ce.Message != "nope" {
		t.Errorf("error: %v", err)
	}
	tctx, tc := context.WithTimeout(ctx, 50*time.Millisecond)
	defer tc()
	if _, err := c.Call(tctx, "NoAnswer", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
	c.Close()
	if _, err := c.Call(ctx, "Echo", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("after close: %v", err)
	}
	<-c.Done()
}

func TestSlowSubscriberKeepsOrder(t *testing.T) {
	c := newPair(t, func(m message) []any {
		var out []any
		for i := 0; i < 50; i++ {
			out = append(out, map[string]any{"method": "E", "params": map[string]any{"i": i}})
		}
		return append(out, map[string]any{"id": m.ID, "result": map[string]any{}})
	})
	var mu sync.Mutex
	var seen []int
	c.Subscribe("E", func(p json.RawMessage) {
		var e struct{ I int }
		json.Unmarshal(p, &e)
		time.Sleep(time.Millisecond)
		mu.Lock()
		seen = append(seen, e.I)
		mu.Unlock()
	})
	if _, err := c.Call(context.Background(), "Go", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for i, v := range seen {
		if v != i {
			t.Fatalf("out of order: %v", seen)
		}
	}
	if len(seen) != 50 {
		t.Errorf("got %d events", len(seen))
	}
}

func TestPipeFraming(t *testing.T) {
	r, w := io.Pipe()
	pc := pipeconn.New(r, w, nil)
	big := strings.Repeat("x", 200000)
	go func() {
		pc.WriteMessage([]byte(`{"a":1}`))
		pc.WriteMessage([]byte(`{"b":"` + big + `"}`))
	}()
	m1, err := pc.ReadMessage()
	if err != nil || string(m1) != `{"a":1}` {
		t.Fatalf("m1: %q %v", m1, err)
	}
	m2, err := pc.ReadMessage()
	if err != nil || len(m2) != len(big)+8 {
		t.Fatalf("m2: %d %v", len(m2), err)
	}
	if pc.WriteMessage([]byte("a\x00b")) == nil {
		t.Error("NUL accepted")
	}
}
