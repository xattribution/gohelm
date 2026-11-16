//go:build !windows

package desktop

import (
	"context"

	"github.com/xattribution/gohelm/policy"
)

// Driver controls other programs. Not available on this platform.
type Driver struct{}

// New returns ErrUnsupported on this platform.
func New(o Options) (*Driver, error) { return nil, ErrUnsupported }

func (d *Driver) Windows(ctx context.Context) ([]Window, int, error) { return nil, 0, ErrUnsupported }
func (d *Driver) Snapshot(ctx context.Context, env policy.Env, window, query string) (Outcome, error) {
	return Outcome{}, ErrUnsupported
}
func (d *Driver) Act(ctx context.Context, env policy.Env, ref, action, value string) (Outcome, error) {
	return Outcome{}, ErrUnsupported
}
func (d *Driver) Screenshot(ctx context.Context, env policy.Env, window string) ([]byte, error) {
	return nil, ErrUnsupported
}
func (d *Driver) Close() {}
