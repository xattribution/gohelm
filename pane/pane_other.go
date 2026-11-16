//go:build !windows

package pane

import (
	"context"

	"github.com/xattribution/gohelm/cdp"
)

// Window is the browser window. Not available on this platform.
type Window struct{}

// Open returns ErrUnsupported on this platform.
func Open(ctx context.Context, o Options) (*Window, error) { return nil, ErrUnsupported }

// Available says whether the window can be opened here.
func Available() bool { return false }

func (w *Window) Transport() cdp.Transport { return nil }
func (w *Window) Show()                    {}
func (w *Window) SetStatus(string)         {}
func (w *Window) Close() error             { return nil }
func (w *Window) Done() <-chan struct{}    { c := make(chan struct{}); close(c); return c }
