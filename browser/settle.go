package browser

import (
	"context"
	"time"
)

// Settling: after an action, wait for the navigation it started and then
// for a short quiet spell with no DOM changes, counted in gohelm's own
// world so the page can't fake it. A page that never goes quiet (a
// spinner, a ticking clock) costs at most quietCap, and the result says
// it's still loading.
const (
	navCap   = 10 * time.Second
	quietCap = 2500 * time.Millisecond
	quietFor = 300 * time.Millisecond
	pollStep = 100 * time.Millisecond
)

// settle waits for the page to settle. It reports whether it's still busy.
func (p *Page) settle(ctx context.Context) (busy bool) {
	sleep(ctx, 60*time.Millisecond)
	// A navigation in progress: wait for it to stop.
	deadline := time.Now().Add(navCap)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		p.mu.Lock()
		loading := p.loading
		p.mu.Unlock()
		if !loading {
			break
		}
		sleep(ctx, pollStep)
	}
	p.mu.Lock()
	stillLoading := p.loading
	p.mu.Unlock()
	if stillLoading {
		return true
	}
	// Then quiet.
	end := time.Now().Add(quietCap)
	last, lastChange := -1.0, time.Now()
	for time.Now().Before(end) && ctx.Err() == nil {
		if p.Dialog() != nil {
			return false
		}
		var st struct {
			M float64 `json:"m"`
		}
		qctx, cancel := context.WithTimeout(ctx, time.Second)
		err := p.evalWorld(qctx, "", "window.__gohelm ? {m: window.__gohelm.m} : {m: -2}", &st)
		cancel()
		if err != nil {
			return false // the document went away: a navigation will say so
		}
		if st.M != last {
			last, lastChange = st.M, time.Now()
		} else if time.Since(lastChange) >= quietFor {
			return false
		}
		sleep(ctx, pollStep)
	}
	return ctx.Err() == nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
