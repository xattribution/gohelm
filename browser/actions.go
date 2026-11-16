package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/xattribution/gohelm/cdp"
	"github.com/xattribution/gohelm/policy"
)

// Outcome is what an action did, for the model and the user.
type Outcome struct {
	// Text is for the model: what happened, then the page's changes or a
	// snapshot. It carries page content, so the host fences it as data.
	Text string
	// Summary is a short line for the user ("Clicked "Search"").
	Summary string
	// Pending is a question the host is showing the user. Stop, and run
	// the same call again after the answer.
	Pending *policy.Ask
	// Refused is set when the action wasn't done.
	Refused bool
}

// ErrDialog means a dialog is open and must be answered first.
var ErrDialog = errors.New("a dialog is open on the page: answer it with browser_dialog first")

// action is one thing the agent does to the page, with its checks.
type action struct {
	tool, verb string
	class      policy.Class
	target     string // role "name", for messages and the audit
	origin     string
}

func (p *Page) begin(ctx context.Context, env policy.Env, a action) (stop *Outcome) {
	p.mu.Lock()
	dlg := p.dialog
	lastEnd := p.actEnd
	p.mu.Unlock()
	if dlg != nil && a.tool != "browser_dialog" {
		return &Outcome{Text: ErrDialog.Error() + fmt.Sprintf(" (%s: %q)", dlg.Type, policy.ShortPageText(dlg.Message, 200)), Summary: "A dialog is open", Refused: true}
	}
	// The user using the page since the agent's last step hands it back.
	// (With a dialog open the page's scripts are paused: nothing to read.)
	if !lastEnd.IsZero() && dlg == nil {
		var st struct {
			Input float64 `json:"input"`
		}
		if p.evalWorld(ctx, "", "window.__gohelm ? {input: window.__gohelm.input} : {input: 0}", &st) == nil && st.Input > 0 {
			at := time.UnixMilli(int64(st.Input))
			if at.After(lastEnd.Add(300 * time.Millisecond)) {
				p.mu.Lock()
				p.actEnd = time.Now()
				p.mu.Unlock()
				if p.o.OnUserActivity != nil {
					p.o.OnUserActivity()
				}
				return &Outcome{Text: "The user used the page themselves since your last step, so nothing was done. Take a new snapshot and check with the user before doing more.", Summary: "Paused: you used the page", Refused: true}
			}
		}
	}
	env.Log(policy.Event{Time: time.Now(), Phase: "intent", Tool: a.tool, Action: a.verb, Class: a.class.String(), Origin: a.origin, Target: a.target})
	p.mu.Lock()
	p.acting = true
	// An approval covers the action it was given for and what the page
	// sends right after it, never a later action.
	if p.approvedNext {
		p.allowSendTill = time.Now().Add(15 * time.Second)
	} else {
		p.allowSendTill = time.Time{}
	}
	p.approvedNext = false
	p.actEnv, p.actCtx, p.actTarget = env, context.WithoutCancel(ctx), a.target
	p.blocked, p.pending, p.cannotAsk = nil, nil, ""
	p.mu.Unlock()
	return nil
}

// finish settles the page and writes the outcome.
func (p *Page) finish(ctx context.Context, env policy.Env, a action, doErr error, said string, gen int) (Outcome, error) {
	busy := false
	if doErr == nil && p.Dialog() == nil {
		busy = p.settle(ctx)
		// Questions the page's requests raised are answered before the
		// result is written (a host's Gate may wait for the user).
		for i := 0; i < 600 && ctx.Err() == nil; i++ {
			p.mu.Lock()
			n := p.inflight
			p.mu.Unlock()
			if n == 0 {
				break
			}
			sleep(ctx, 50*time.Millisecond)
		}
	}
	p.mu.Lock()
	p.acting = false
	p.actEnd = time.Now().Add(1500 * time.Millisecond)
	pending, cannot, blocked := p.pending, p.cannotAsk, p.blocked
	p.mu.Unlock()

	out := Outcome{Summary: said}
	var b strings.Builder
	if doErr != nil {
		out.Refused = true
		b.WriteString("Not done: " + doErr.Error() + "\n")
	} else {
		b.WriteString(said + ".\n")
	}
	if pending != nil {
		out.Pending = pending
		fmt.Fprintf(&b, "The page tried something that needs the user's OK, and nothing was sent: %s The user is being asked. When they answer, call the same tool again with the same arguments.\n", pending.Title)
	}
	if cannot != "" {
		b.WriteString("Stopped: " + cannot + ".\n")
	}
	for _, r := range blocked {
		fmt.Fprintf(&b, "Blocked: %s %s (the page tried to send data after this action).\n", r.method, shortURL(r.url))
	}
	for _, n := range p.takeNotes() {
		b.WriteString(n + "\n")
	}
	if busy {
		b.WriteString("The page is still loading or changing.\n")
	}
	if d := p.Dialog(); d != nil {
		fmt.Fprintf(&b, "A %s dialog is open: %q. Answer it with browser_dialog (accept or dismiss) before anything else.\n", d.Type, policy.ShortPageText(d.Message, 300))
	} else if doErr == nil || errors.Is(doErr, errGone) {
		b.WriteString(p.changes(ctx, gen))
	}
	out.Text = strings.TrimRight(b.String(), "\n")
	res := "done"
	if out.Refused {
		res = "not done"
	}
	if out.Pending != nil {
		res = "waiting for the user"
	}
	ev := policy.Event{Time: time.Now(), Phase: "result", Tool: a.tool, Action: a.verb, Class: a.class.String(), Origin: a.origin, Target: a.target, Outcome: res}
	if doErr != nil {
		ev.Error = doErr.Error()
	}
	env.Log(ev)
	return out, nil
}

// changes is what changed since the last snapshot, or a fresh snapshot
// after a new page or a big change.
func (p *Page) changes(ctx context.Context, gen int) string {
	p.mu.Lock()
	prev := p.lastSnap
	newDoc := p.gen != gen
	p.mu.Unlock()
	snap, err := p.Snapshot(ctx, SnapOptions{})
	if err != nil {
		return "(Couldn't read the page afterwards: " + err.Error() + ")\n"
	}
	if newDoc || prev == nil || prev.gen != snap.gen {
		return "Now on a new page:\n" + snap.Text
	}
	var added, changed, removed []string
	for _, id := range snap.Order {
		l := snap.Lines[id]
		if old, ok := prev.Lines[id]; !ok {
			added = append(added, "+ "+l[2:])
		} else if old != l {
			changed = append(changed, "~ "+l[2:])
		}
	}
	for _, id := range prev.Order {
		if _, ok := snap.Lines[id]; !ok {
			removed = append(removed, "- "+prev.Lines[id][2:])
		}
	}
	n := len(added) + len(changed) + len(removed)
	head := fmt.Sprintf("Page: %s\nURL: %s\n", policy.ShortPageText(snap.Title, 120), shortURL(snap.URL))
	if n == 0 {
		return head + "No change to what can be acted on. (browser_read shows the page's text.)\n"
	}
	if n > 40 {
		return "The page changed a lot:\n" + snap.Text
	}
	var b strings.Builder
	b.WriteString(head + "Changes:\n")
	for _, l := range append(append(changed, added...), removed...) {
		b.WriteString(l + "\n")
	}
	// Notes at the end of the snapshot still apply.
	if i := strings.Index(snap.Text, "\nLeft out:"); i >= 0 {
		b.WriteString(snap.Text[i+1:])
	} else if i := strings.Index(snap.Text, "\nNote:"); i >= 0 {
		b.WriteString(snap.Text[i+1:])
	}
	return b.String()
}

func (p *Page) generation() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen
}

func label(role, name string) string {
	if name == "" {
		return role
	}
	return fmt.Sprintf("%s %q", role, policy.ShortPageText(name, 60))
}

// mayAct checks that the agent may act on the page's site, asking the user
// when the rules say to. It returns a stop outcome when it may not (yet).
func (p *Page) mayAct(ctx context.Context, env policy.Env, class policy.Class) *Outcome {
	if class == policy.Read {
		return nil
	}
	u := p.URL()
	v, g := p.grant(u)
	switch {
	case v.Verdict == policy.Refused || g == policy.GrantDenied:
		return &Outcome{Text: "Not done: the assistant may not act on " + v.Host + ".", Summary: "Not allowed on this site", Refused: true}
	case v.Verdict == policy.ReadOnly:
		return &Outcome{Text: "Not done: " + v.Host + " is read-only for the assistant. It can read and follow links there, never type or submit.", Summary: "Read-only site", Refused: true}
	case g == policy.GrantAct:
		return nil
	}
	ask := policy.Ask{Kind: policy.KindSite, Key: policy.MakeKey(policy.KindSite, "act", v.Origin), Origin: v.Origin, URL: u,
		Title: "Let " + p.o.AgentName + " type and click on " + v.Host + "?", Options: []policy.Option{policy.OptAllow, policy.OptAllowSite, policy.OptDeny}}
	a, err := env.Ask(ctx, ask)
	return p.answered(ask, a, err, func(site bool) { p.setGrant(v.Origin, policy.GrantAct, site) })
}

// answered turns a Gate answer into a stop outcome, or nil to go on (after
// calling yes).
func (p *Page) answered(ask policy.Ask, a policy.Answer, err error, yes func(site bool)) *Outcome {
	switch {
	case err != nil:
		return &Outcome{Text: "Not done: couldn't ask the user: " + err.Error(), Summary: "Couldn't ask you", Refused: true}
	case a == policy.Allow || a == policy.Done:
		if yes != nil {
			yes(false)
		}
		return nil
	case a == policy.AllowSite:
		if yes != nil {
			yes(true)
		}
		return nil
	case a == policy.Pending:
		return &Outcome{Text: "Waiting for the user: " + ask.Title + " When they answer, call the same tool again with the same arguments.", Summary: "Asks you first", Pending: &ask}
	case a == policy.CannotAsk:
		return &Outcome{Text: "Not done: it needs the user's OK (" + ask.Title + "), and nobody can answer in this run. Say so in the answer.", Summary: "Needs your OK", Refused: true}
	default:
		return &Outcome{Text: "Not done: the user said no (" + ask.Title + "). Don't try another way.", Summary: "You said no", Refused: true}
	}
}

// ---- open, back, forward, reload ----

// Navigate opens url in the page.
func (p *Page) Navigate(ctx context.Context, env policy.Env, raw string) (Outcome, error) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") && !strings.HasPrefix(raw, "about:") {
		raw = "https://" + raw
	}
	v, g := p.grant(raw)
	a := action{tool: "browser_go", verb: "open", class: policy.Read, target: shortURL(raw), origin: v.Origin}
	switch {
	case v.Verdict == policy.Refused:
		env.Log(policy.Event{Time: time.Now(), Phase: "result", Tool: a.tool, Action: a.verb, Origin: v.Origin, Outcome: "refused", Error: v.Reason})
		return Outcome{Text: "Not opened: " + v.Reason + ".", Summary: "Not opened", Refused: true}, nil
	case g == policy.GrantDenied:
		return Outcome{Text: "Not opened: the user said no to " + v.Host + ".", Summary: "Not opened", Refused: true}, nil
	case v.Verdict == policy.AskUser && g == policy.NoGrant:
		ask := policy.Ask{Kind: policy.KindSite, Key: policy.MakeKey(policy.KindSite, v.Origin), Origin: v.Origin, URL: raw,
			Title: "Let " + p.o.AgentName + " open " + v.Host + "?", Detail: []string{"Address: " + raw}, Options: []policy.Option{policy.OptAllow, policy.OptAllowSite, policy.OptDeny}}
		ans, err := env.Ask(ctx, ask)
		if stop := p.answered(ask, ans, err, func(site bool) { p.setGrant(v.Origin, policy.GrantAct, site) }); stop != nil {
			return *stop, nil
		}
	}
	gen := p.generation()
	if stop := p.begin(ctx, env, a); stop != nil {
		return *stop, nil
	}
	var r struct {
		ErrorText string `json:"errorText"`
	}
	err := cdp.Do(ctx, p.t, "Page.navigate", map[string]any{"url": raw}, &r)
	if err == nil && r.ErrorText != "" {
		err = fmt.Errorf("the page didn't open (%s)", r.ErrorText)
	}
	return p.finish(ctx, env, a, err, "Opened "+shortURL(raw), gen)
}

// History goes back, forward, or reloads.
func (p *Page) History(ctx context.Context, env policy.Env, verb string) (Outcome, error) {
	a := action{tool: "browser_go", verb: verb, class: policy.Read, origin: p.Origin()}
	gen := p.generation()
	var hist struct {
		Current int `json:"currentIndex"`
		Entries []struct {
			ID  int    `json:"id"`
			URL string `json:"url"`
		} `json:"entries"`
	}
	var entry int
	switch verb {
	case "back", "forward":
		if err := cdp.Do(ctx, p.t, "Page.getNavigationHistory", nil, &hist); err != nil {
			return Outcome{}, err
		}
		i := hist.Current - 1
		if verb == "forward" {
			i = hist.Current + 1
		}
		if i < 0 || i >= len(hist.Entries) {
			return Outcome{Text: "There's no page to go " + verb + " to.", Summary: "Nothing to go " + verb + " to", Refused: true}, nil
		}
		v, g := p.grant(hist.Entries[i].URL)
		if v.Verdict == policy.Refused || g == policy.GrantDenied || (v.Verdict == policy.AskUser && g == policy.NoGrant) {
			return Outcome{Text: "Not done: going " + verb + " would open " + v.Host + ", which the assistant may not open now.", Summary: "Not allowed", Refused: true}, nil
		}
		entry = hist.Entries[i].ID
		a.target = shortURL(hist.Entries[i].URL)
	case "reload":
	default:
		return Outcome{}, fmt.Errorf("unknown action %q", verb)
	}
	if stop := p.begin(ctx, env, a); stop != nil {
		return *stop, nil
	}
	var err error
	if verb == "reload" {
		_, err = p.t.Call(ctx, "Page.reload", nil)
	} else {
		_, err = p.t.Call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": entry})
	}
	said := map[string]string{"back": "Went back", "forward": "Went forward", "reload": "Reloaded the page"}[verb]
	return p.finish(ctx, env, a, err, said, gen)
}

// ---- click ----

type elemInfo struct {
	Tag      string `json:"tag"`
	Type     string `json:"type"`
	Submit   bool   `json:"submit"`
	Action   string `json:"action"`
	Method   string `json:"method"`
	Href     string `json:"href"`
	Disabled bool   `json:"disabled"`
}

const elemInfoJS = `function() {
  const tag = (this.tagName || '').toLowerCase();
  const type = ((this.getAttribute && this.getAttribute('type')) || '').toLowerCase();
  const f = this.form || (this.closest && this.closest('form')) || null;
  const submit = !!f && ((tag === 'button' && (type === '' || type === 'submit')) || (tag === 'input' && (type === 'submit' || type === 'image')));
  return {tag, type, submit, action: f ? String(f.action || '') : '', method: f ? String(f.method || 'get') : '',
    href: typeof this.href === 'string' ? this.href : '', disabled: !!this.disabled || this.getAttribute && this.getAttribute('aria-disabled') === 'true'};
}`

// current re-reads a ref's role and name at action time, so a page that
// swapped the element since the snapshot can't borrow the old name.
func (p *Page) current(ctx context.Context, t refTarget) (refTarget, error) {
	var r struct {
		Nodes []*axNode `json:"nodes"`
	}
	if err := cdp.Do(ctx, p.t, "Accessibility.getPartialAXTree", map[string]any{"backendNodeId": t.backend, "fetchRelatives": false}, &r); err != nil {
		return t, errGone
	}
	for _, n := range r.Nodes {
		if n.Backend == t.backend {
			t.role, t.name = n.role(), n.name()
			break
		}
	}
	return t, nil
}

// dialogStop is the outcome of an action tried while a dialog is open.
func (p *Page) dialogStop() *Outcome {
	d := p.Dialog()
	if d == nil {
		return nil
	}
	return &Outcome{Text: ErrDialog.Error() + fmt.Sprintf(" (%s: %q)", d.Type, policy.ShortPageText(d.Message, 200)), Summary: "A dialog is open", Refused: true}
}

// Click clicks an element.
func (p *Page) Click(ctx context.Context, env policy.Env, ref string, double bool) (Outcome, error) {
	if stop := p.dialogStop(); stop != nil {
		return *stop, nil
	}
	t, err := p.ref(ref)
	if err != nil {
		return Outcome{Text: "Not done: " + err.Error(), Summary: "Element not found", Refused: true}, nil
	}
	gen := t.gen
	if t, err = p.current(ctx, t); err != nil {
		return Outcome{Text: "Not done: " + err.Error(), Summary: "Element gone", Refused: true}, nil
	}
	var info elemInfo
	if err := p.callOn(ctx, t, elemInfoJS, &info); err != nil {
		return Outcome{Text: "Not done: " + err.Error(), Summary: "Element gone", Refused: true}, nil
	}
	class := policy.ClickClass(t.role, t.name, info.Submit)
	a := action{tool: "browser_click", verb: "click", class: class, target: label(t.role, t.name), origin: p.Origin()}
	if info.Disabled {
		return Outcome{Text: "Not done: " + a.target + " is disabled.", Summary: "Disabled", Refused: true}, nil
	}
	if stop := p.mayAct(ctx, env, class); stop != nil {
		return *stop, nil
	}
	if class == policy.Commit {
		if stop := p.confirmCommit(ctx, env, a, t, info); stop != nil {
			return *stop, nil
		}
	}
	if stop := p.begin(ctx, env, a); stop != nil {
		return *stop, nil
	}
	err = p.clickTarget(ctx, t, double)
	verb := "Clicked "
	if double {
		verb = "Double-clicked "
	}
	return p.finish(ctx, env, a, err, verb+a.target, gen)
}

// confirmCommit asks the Critic and the user before an action that sends
// or changes something. An approval lets what the page sends right after
// it through.
func (p *Page) confirmCommit(ctx context.Context, env policy.Env, a action, t refTarget, info elemInfo) *Outcome {
	if env.Critic != nil {
		ok, why, err := env.Critic.Review(ctx, policy.Proposal{Task: env.Task, Class: a.class, Action: a.verb, Origin: a.origin,
			Role: t.role, Name: policy.ShortPageText(t.name, 80)})
		if err == nil && !ok {
			env.Log(policy.Event{Time: time.Now(), Phase: "result", Tool: a.tool, Action: a.verb, Class: a.class.String(), Origin: a.origin, Target: a.target, Decision: "critic veto", Outcome: "not done"})
			return &Outcome{Text: "Not done: the check of this action against the user's request stopped it (" + why + "). Don't try it another way; ask the user.", Summary: "Stopped by the check", Refused: true}
		}
	}
	p.mu.Lock()
	siteOK := p.sendOK[a.origin]
	cardSite := p.cardSites[a.origin]
	p.mu.Unlock()
	if siteOK || !p.o.Rules.NeedsOK(a.class, cardSite) {
		p.mu.Lock()
		p.approvedNext = true
		p.mu.Unlock()
		return nil
	}
	detail := []string{}
	if info.Action != "" {
		detail = append(detail, "Sends to: "+shortURL(info.Action)+" ("+strings.ToUpper(info.Method)+")")
	} else if info.Href != "" && !strings.HasPrefix(info.Href, "javascript:") {
		detail = append(detail, "Opens: "+shortURL(info.Href))
	}
	detail = append(detail, p.filledLines()...)
	ask := policy.Ask{Kind: policy.KindCommit, Key: policy.MakeKey(policy.KindCommit, a.origin, t.role, t.name, info.Action, p.filledDigest()),
		Origin: a.origin, URL: p.URL(), Title: fmt.Sprintf("%s %s on %s?", strings.ToUpper(a.verb[:1])+a.verb[1:], a.target, hostOf(p.URL())),
		Detail: detail, Options: []policy.Option{policy.OptAllow, policy.OptAllowSite, policy.OptDeny}}
	ans, err := env.Ask(ctx, ask)
	return p.answered(ask, ans, err, func(site bool) {
		p.mu.Lock()
		p.approvedNext = true
		if site {
			p.sendOK[a.origin] = true
		}
		p.mu.Unlock()
	})
}

// clickTarget scrolls the element into view, checks nothing covers it, and
// clicks it with the mouse.
func (p *Page) clickTarget(ctx context.Context, t refTarget, double bool) error {
	p.t.Call(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": t.backend})
	x, y, err := p.clickPoint(ctx, t)
	if err != nil {
		return err
	}
	if err := p.input(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y}); err != nil {
		return err
	}
	clicks := 1
	if double {
		clicks = 2
	}
	for c := 1; c <= clicks; c++ {
		if err := p.input(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": c}); err != nil {
			return err
		}
		if err := p.input(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "buttons": 0, "clickCount": c}); err != nil {
			return err
		}
		if p.Dialog() != nil {
			return nil
		}
	}
	return nil
}

const hitJS = `function(h) {
  if (!h) return false;
  if (this === h || this.contains(h)) return true;
  if (h.control === this) return true;
  if (this.labels) for (const l of this.labels) if (l === h || l.contains(h)) return true;
  let r = h.getRootNode && h.getRootNode();
  while (r && r.host) { if (this === r.host || this.contains(r.host)) return true; r = r.host.getRootNode(); }
  return false;
}`

// clickPoint finds a point on the element that reaches it: the middle of
// each of its boxes, then points inside them.
func (p *Page) clickPoint(ctx context.Context, t refTarget) (float64, float64, error) {
	var q struct {
		Quads [][]float64 `json:"quads"`
	}
	if err := cdp.Do(ctx, p.t, "DOM.getContentQuads", map[string]any{"backendNodeId": t.backend}, &q); err != nil || len(q.Quads) == 0 {
		return 0, 0, fmt.Errorf("%s isn't visible on the page", label(t.role, t.name))
	}
	var pts [][2]float64
	for _, quad := range q.Quads {
		if len(quad) != 8 {
			continue
		}
		cx := (quad[0] + quad[2] + quad[4] + quad[6]) / 4
		cy := (quad[1] + quad[3] + quad[5] + quad[7]) / 4
		pts = append(pts, [2]float64{cx, cy})
		for _, f := range []float64{0.25, 0.75} {
			pts = append(pts, [2]float64{quad[0] + (quad[4]-quad[0])*f, quad[1] + (quad[5]-quad[1])*f})
		}
	}
	var cover int
	for _, pt := range pts {
		if pt[0] < 0 || pt[1] < 0 || math.IsNaN(pt[0]) {
			continue
		}
		var hit struct {
			Backend int `json:"backendNodeId"`
		}
		if err := cdp.Do(ctx, p.t, "DOM.getNodeForLocation", map[string]any{"x": int(pt[0]), "y": int(pt[1]), "includeUserAgentShadowDOM": true, "ignorePointerEventsNone": false}, &hit); err != nil {
			continue
		}
		if hit.Backend == t.backend {
			return pt[0], pt[1], nil
		}
		obj, err := p.resolveIn(ctx, t.frame, hit.Backend)
		if err != nil {
			cover = hit.Backend
			continue
		}
		var ok bool
		err = p.callOn(ctx, t, hitJS, &ok, obj)
		p.t.Call(context.WithoutCancel(ctx), "Runtime.releaseObject", map[string]any{"objectId": string(obj)})
		if err == nil && ok {
			return pt[0], pt[1], nil
		}
		cover = hit.Backend
	}
	if cover != 0 {
		return 0, 0, fmt.Errorf("%s is covered by %s; deal with that first (close it, or scroll)", label(t.role, t.name), p.describe(ctx, cover))
	}
	return 0, 0, fmt.Errorf("%s can't be reached with the mouse", label(t.role, t.name))
}

// describe names an element (or its nearest named ancestor) for a message.
func (p *Page) describe(ctx context.Context, backend int) string {
	var r struct {
		Nodes []*axNode `json:"nodes"`
	}
	if cdp.Do(ctx, p.t, "Accessibility.getPartialAXTree", map[string]any{"backendNodeId": backend, "fetchRelatives": true}, &r) == nil {
		// Ancestors come after the node; take the nearest one with a name.
		for _, n := range r.Nodes {
			if n.name() != "" && !n.Ignored && n.role() != "StaticText" && n.role() != "RootWebArea" {
				d := label(n.role(), n.name())
				if ref, ok := p.refFor(n.Backend); ok {
					d += " [ref=" + ref + "]"
				}
				return d
			}
		}
	}
	return "another element"
}

func (p *Page) refFor(backend int) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.byNode[backend]
	return r, ok
}

// ---- fill ----

// FieldValue is one field to fill.
type FieldValue struct {
	Ref   string `json:"ref"`
	Value string `json:"value"`
}

type fieldInfo struct {
	Tag      string     `json:"tag"`
	Type     string     `json:"type"`
	AC       string     `json:"ac"`
	Name     string     `json:"name"`
	ID       string     `json:"id"`
	PH       string     `json:"ph"`
	CE       bool       `json:"ce"`
	Checked  bool       `json:"checked"`
	Disabled bool       `json:"disabled"`
	ReadOnly bool       `json:"readonly"`
	Options  [][]string `json:"options"`
}

const fieldInfoJS = `function() {
  const tag = (this.tagName || '').toLowerCase();
  return {tag, type: String(this.type || '').toLowerCase(), ac: this.getAttribute('autocomplete') || '', name: this.getAttribute('name') || '',
    id: this.id || '', ph: this.getAttribute('placeholder') || '', ce: !!this.isContentEditable, checked: !!this.checked,
    disabled: !!this.disabled || this.getAttribute('aria-disabled') === 'true', readonly: !!this.readOnly || this.getAttribute('aria-readonly') === 'true',
    options: tag === 'select' ? Array.from(this.options).slice(0, 200).map(o => [o.value, o.label || o.text]) : null};
}`

const selectJS = `function(v) {
  const opts = Array.from(this.options);
  const lv = String(v).toLowerCase();
  let o = opts.find(o => (o.label || o.text) === v) || opts.find(o => o.value === v) ||
          opts.find(o => (o.label || o.text).toLowerCase() === lv) || opts.find(o => (o.label || o.text).toLowerCase().includes(lv));
  if (!o) return {ok: false, options: opts.slice(0, 30).map(o => o.label || o.text)};
  this.value = o.value; o.selected = true;
  this.dispatchEvent(new Event('input', {bubbles: true}));
  this.dispatchEvent(new Event('change', {bubbles: true}));
  return {ok: true, chosen: o.label || o.text};
}`

const setValueJS = `function(v) {
  const proto = Object.getPrototypeOf(this);
  const d = Object.getOwnPropertyDescriptor(proto, 'value');
  if (d && d.set) d.set.call(this, v); else this.value = v;
  this.dispatchEvent(new Event('input', {bubbles: true}));
  this.dispatchEvent(new Event('change', {bubbles: true}));
  return String(this.value);
}`

const selectAllJS = `function() {
  if (this.isContentEditable) {
    const r = document.createRange(); r.selectNodeContents(this);
    const s = getSelection(); s.removeAllRanges(); s.addRange(r);
  } else if (typeof this.select === 'function') { this.select(); }
}`

// Fill types values into fields, chooses options and sets checkboxes.
func (p *Page) Fill(ctx context.Context, env policy.Env, fields []FieldValue) (Outcome, error) {
	if stop := p.dialogStop(); stop != nil {
		return *stop, nil
	}
	if len(fields) == 0 {
		return Outcome{Text: "Not done: no fields given.", Refused: true}, nil
	}
	if len(fields) > 30 {
		return Outcome{Text: "Not done: at most 30 fields at a time.", Refused: true}, nil
	}
	type plan struct {
		fv   FieldValue
		t    refTarget
		info fieldInfo
	}
	var plans []plan
	gen := p.generation()
	for _, fv := range fields {
		t, err := p.ref(fv.Ref)
		if err != nil {
			return Outcome{Text: "Not done: " + err.Error(), Summary: "Field not found", Refused: true}, nil
		}
		if t, err = p.current(ctx, t); err != nil {
			return Outcome{Text: "Not done: " + fv.Ref + ": " + err.Error(), Summary: "Field gone", Refused: true}, nil
		}
		var info fieldInfo
		if err := p.callOn(ctx, t, fieldInfoJS, &info); err != nil {
			return Outcome{Text: "Not done: " + fv.Ref + ": " + err.Error(), Summary: "Field gone", Refused: true}, nil
		}
		if policy.SecretField(policy.Field{Tag: info.Tag, Type: info.Type, Autocomplete: info.AC, Name: info.Name, ID: info.ID, Label: t.name, Placeholder: info.PH}) {
			env.Log(policy.Event{Time: time.Now(), Phase: "result", Tool: "browser_fill", Action: "fill", Class: policy.Secret.String(), Origin: p.Origin(), Target: label(t.role, t.name), Outcome: "refused: secret field"})
			return Outcome{Text: fmt.Sprintf("Not done: %s is a password, PIN, code or ID field. The assistant never types those: ask the user to type it themselves (browser_hand_over).", label(t.role, t.name)), Summary: "You type this one", Refused: true}, nil
		}
		if info.Disabled || info.ReadOnly {
			return Outcome{Text: "Not done: " + label(t.role, t.name) + " can't be changed (disabled or read-only).", Summary: "Field locked", Refused: true}, nil
		}
		plans = append(plans, plan{fv, t, info})
	}
	if stop := p.mayAct(ctx, env, policy.Input); stop != nil {
		return *stop, nil
	}
	origin := p.Origin()
	p.mu.Lock()
	cardSite := p.cardSites[origin]
	p.mu.Unlock()
	if p.o.Rules.NeedsOK(policy.Input, cardSite) {
		var detail, keyParts []string
		for _, pl := range plans {
			detail = append(detail, pl.t.name+" = "+policy.ShortPageText(pl.fv.Value, 60))
			keyParts = append(keyParts, pl.t.name, pl.fv.Value)
		}
		ask := policy.Ask{Kind: policy.KindInput, Key: policy.MakeKey(policy.KindInput, append([]string{origin}, keyParts...)...), Origin: origin, URL: p.URL(),
			Title: fmt.Sprintf("Fill in %d field(s) on %s?", len(plans), hostOf(p.URL())), Detail: detail, Options: []policy.Option{policy.OptAllow, policy.OptAllowSite, policy.OptDeny}}
		ans, err := env.Ask(ctx, ask)
		if stop := p.answered(ask, ans, err, nil); stop != nil {
			return *stop, nil
		}
	}
	a := action{tool: "browser_fill", verb: "fill", class: policy.Input, target: fmt.Sprintf("%d field(s)", len(plans)), origin: origin}
	if stop := p.begin(ctx, env, a); stop != nil {
		return *stop, nil
	}
	var done []string
	var ferr error
	for _, pl := range plans {
		if err := p.fillOne(ctx, pl.t, pl.info, pl.fv.Value); err != nil {
			ferr = fmt.Errorf("%s: %w", label(pl.t.role, pl.t.name), err)
			break
		}
		key := pl.t.name
		if key == "" {
			key = pl.fv.Ref
		}
		p.mu.Lock()
		p.filled[key] = pl.fv.Value
		p.mu.Unlock()
		done = append(done, label(pl.t.role, pl.t.name))
	}
	said := "Filled " + strings.Join(done, ", ")
	if len(done) == 0 {
		said = "Filled nothing"
	}
	return p.finish(ctx, env, a, ferr, said, gen)
}

func (p *Page) fillOne(ctx context.Context, t refTarget, info fieldInfo, value string) error {
	switch {
	case info.Tag == "input" && (info.Type == "checkbox" || info.Type == "radio"):
		want, ok := parseBool(value)
		if !ok {
			return fmt.Errorf("give true or false for a checkbox, not %q", value)
		}
		if info.Type == "radio" && !want {
			return errors.New("a radio button is turned off by choosing another one")
		}
		if info.Checked != want {
			return p.clickTarget(ctx, t, false)
		}
		return nil
	case info.Tag == "select":
		var r struct {
			OK      bool     `json:"ok"`
			Chosen  string   `json:"chosen"`
			Options []string `json:"options"`
		}
		if err := p.callOn(ctx, t, selectJS, &r, value); err != nil {
			return err
		}
		if !r.OK {
			return fmt.Errorf("no option matches %q; the options are: %s", value, strings.Join(r.Options, "; "))
		}
		return nil
	case info.Tag == "input" && (info.Type == "date" || info.Type == "datetime-local" || info.Type == "month" || info.Type == "week" || info.Type == "time" || info.Type == "color" || info.Type == "range"):
		var got string
		if err := p.callOn(ctx, t, setValueJS, &got, value); err != nil {
			return err
		}
		if got != value {
			return fmt.Errorf("the field didn't take %q (it holds %q); check the format it expects", value, got)
		}
		return nil
	case info.Tag == "input" || info.Tag == "textarea" || info.CE:
		if info.Tag == "input" {
			value = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(value)
		}
		if _, err := p.t.Call(ctx, "DOM.focus", map[string]any{"backendNodeId": t.backend}); err != nil {
			return err
		}
		if err := p.callOn(ctx, t, selectAllJS, nil); err != nil {
			return err
		}
		if value == "" {
			return p.sendKey(ctx, "Delete")
		}
		_, err := p.t.Call(ctx, "Input.insertText", map[string]any{"text": value})
		return err
	}
	return fmt.Errorf("it isn't a field that takes text, a choice or a checkmark (it's a %s)", info.Tag)
}

func parseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "checked", "1", "check", "x":
		return true, true
	case "false", "no", "off", "unchecked", "0", "uncheck", "":
		return false, true
	}
	return false, false
}

// ---- press, scroll ----

// Press presses a key, on a ref's element when given.
func (p *Page) Press(ctx context.Context, env policy.Env, key, ref string) (Outcome, error) {
	if stop := p.dialogStop(); stop != nil {
		return *stop, nil
	}
	key = normalizeKey(strings.TrimSpace(key))
	if !policy.AllowedKeys[key] {
		return Outcome{Text: fmt.Sprintf("Not done: %q isn't a key the assistant presses. Allowed: Enter, Tab, Shift+Tab, Escape, Backspace, Delete, Space, the arrows, Home, End, PageUp, PageDown, Control+A.", key), Refused: true}, nil
	}
	class := keyClass(key)
	gen := p.generation()
	target := "the page"
	var t refTarget
	if ref != "" {
		var err error
		if t, err = p.ref(ref); err != nil {
			return Outcome{Text: "Not done: " + err.Error(), Refused: true}, nil
		}
		if t, err = p.current(ctx, t); err != nil {
			return Outcome{Text: "Not done: " + err.Error(), Refused: true}, nil
		}
		target = label(t.role, t.name)
		if key == "Enter" || key == "Space" {
			var info elemInfo
			if p.callOn(ctx, t, elemInfoJS, &info) == nil && policy.ClickClass(t.role, t.name, info.Submit) == policy.Commit {
				class = policy.Commit
			}
		}
	}
	a := action{tool: "browser_press", verb: "press " + key, class: class, target: target, origin: p.Origin()}
	if stop := p.mayAct(ctx, env, class); stop != nil {
		return *stop, nil
	}
	if class == policy.Commit {
		if stop := p.confirmCommit(ctx, env, a, t, elemInfo{}); stop != nil {
			return *stop, nil
		}
	}
	if stop := p.begin(ctx, env, a); stop != nil {
		return *stop, nil
	}
	var err error
	if ref != "" {
		_, err = p.t.Call(ctx, "DOM.focus", map[string]any{"backendNodeId": t.backend})
	}
	if err == nil {
		err = p.sendKey(ctx, key)
	}
	return p.finish(ctx, env, a, err, "Pressed "+key+" on "+target, gen)
}

// Scroll scrolls the page, or brings a ref's element into view.
func (p *Page) Scroll(ctx context.Context, env policy.Env, direction, ref string) (Outcome, error) {
	gen := p.generation()
	a := action{tool: "browser_go", verb: "scroll", class: policy.Read, origin: p.Origin()}
	if stop := p.begin(ctx, env, a); stop != nil {
		return *stop, nil
	}
	var err error
	said := "Scrolled " + direction
	if ref != "" {
		var t refTarget
		if t, err = p.ref(ref); err == nil {
			_, err = p.t.Call(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": t.backend})
			said = "Scrolled to " + label(t.role, t.name)
		}
	} else {
		var m struct {
			V struct {
				W float64 `json:"clientWidth"`
				H float64 `json:"clientHeight"`
			} `json:"cssVisualViewport"`
		}
		if err = cdp.Do(ctx, p.t, "Page.getLayoutMetrics", nil, &m); err == nil {
			dx, dy := 0.0, 0.0
			switch direction {
			case "up":
				dy = -0.8 * m.V.H
			case "left":
				dx = -0.8 * m.V.W
			case "right":
				dx = 0.8 * m.V.W
			default:
				dy = 0.8 * m.V.H
				said = "Scrolled down"
			}
			_, err = p.t.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": m.V.W / 2, "y": m.V.H / 2, "deltaX": dx, "deltaY": dy})
		}
	}
	return p.finish(ctx, env, a, err, said, gen)
}

// ---- dialogs ----

// HandleDialog answers the open dialog.
func (p *Page) HandleDialog(ctx context.Context, env policy.Env, accept bool, text string) (Outcome, error) {
	p.mu.Lock()
	d := p.dialog
	p.mu.Unlock()
	if d == nil {
		return Outcome{Text: "There's no dialog open.", Refused: true}, nil
	}
	class := policy.Read
	switch {
	case accept && d.Type == "confirm":
		class = policy.Commit
	case accept && d.Type == "prompt":
		class = policy.Input
	}
	a := action{tool: "browser_dialog", verb: map[bool]string{true: "accept", false: "dismiss"}[accept], class: class,
		target: fmt.Sprintf("%s %q", d.Type, policy.ShortPageText(d.Message, 80)), origin: p.Origin()}
	if stop := p.mayAct(ctx, env, class); stop != nil {
		return *stop, nil
	}
	if class == policy.Commit {
		p.mu.Lock()
		siteOK := p.sendOK[a.origin]
		p.mu.Unlock()
		if !siteOK {
			ask := policy.Ask{Kind: policy.KindCommit, Key: policy.MakeKey(policy.KindCommit, a.origin, "dialog", d.Message), Origin: a.origin, URL: p.URL(),
				Title: fmt.Sprintf("Answer OK to %q on %s?", policy.ShortPageText(d.Message, 120), hostOf(p.URL())), Options: []policy.Option{policy.OptAllow, policy.OptDeny}}
			ans, err := env.Ask(ctx, ask)
			if stop := p.answered(ask, ans, err, func(bool) {
				p.mu.Lock()
				p.approvedNext = true
				p.mu.Unlock()
			}); stop != nil {
				return *stop, nil
			}
		}
	}
	gen := p.generation()
	if stop := p.begin(ctx, env, a); stop != nil {
		return *stop, nil
	}
	params := map[string]any{"accept": accept}
	if d.Type == "prompt" && accept {
		params["promptText"] = text
	}
	err := p.input(ctx, "Page.handleJavaScriptDialog", params)
	if err == nil {
		p.mu.Lock()
		p.dialog = nil
		p.mu.Unlock()
	}
	return p.finish(ctx, env, a, err, map[bool]string{true: "Answered OK", false: "Dismissed"}[accept]+" to the "+d.Type, gen)
}

// ---- wait, screenshot ----

// WaitFor waits until text appears on the page (or, with gone, until it's
// gone), up to timeout.
func (p *Page) WaitFor(ctx context.Context, text string, gone bool, timeout time.Duration) (bool, error) {
	if timeout <= 0 || timeout > 20*time.Second {
		timeout = 10 * time.Second
	}
	b, _ := json.Marshal(strings.ToLower(text))
	expr := "(document.body ? document.body.innerText.toLowerCase().includes(" + string(b) + ") : false)"
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		var has bool
		if err := p.evalWorld(ctx, "", expr, &has); err == nil && has != gone {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		sleep(ctx, 250*time.Millisecond)
	}
	return false, nil
}

// Screenshot takes a JPEG picture of the page's viewport, or of one
// element.
func (p *Page) Screenshot(ctx context.Context, ref string) ([]byte, error) {
	params := map[string]any{"format": "jpeg", "quality": 70}
	if ref != "" {
		t, err := p.ref(ref)
		if err != nil {
			return nil, err
		}
		p.t.Call(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": t.backend})
		var bm struct {
			Model struct {
				Border []float64 `json:"border"`
			} `json:"model"`
		}
		if err := cdp.Do(ctx, p.t, "DOM.getBoxModel", map[string]any{"backendNodeId": t.backend}, &bm); err != nil || len(bm.Model.Border) != 8 {
			return nil, fmt.Errorf("%s isn't visible", ref)
		}
		var m struct {
			V struct {
				PageX float64 `json:"pageX"`
				PageY float64 `json:"pageY"`
			} `json:"cssVisualViewport"`
		}
		cdp.Do(ctx, p.t, "Page.getLayoutMetrics", nil, &m)
		q := bm.Model.Border
		minX, minY := math.Min(math.Min(q[0], q[2]), math.Min(q[4], q[6])), math.Min(math.Min(q[1], q[3]), math.Min(q[5], q[7]))
		maxX, maxY := math.Max(math.Max(q[0], q[2]), math.Max(q[4], q[6])), math.Max(math.Max(q[1], q[3]), math.Max(q[5], q[7]))
		params["clip"] = map[string]any{"x": minX + m.V.PageX, "y": minY + m.V.PageY, "width": math.Max(1, maxX-minX), "height": math.Max(1, maxY-minY), "scale": 1}
	}
	var r struct {
		Data []byte `json:"data"`
	}
	if err := cdp.Do(ctx, p.t, "Page.captureScreenshot", params, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

// Dialog is the open dialog, if any.
func (p *Page) Dialog() *Dialog {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dialog == nil {
		return nil
	}
	d := *p.dialog
	return &d
}

// input sends an input event. An event whose handler opens a dialog
// doesn't get its answer until the dialog closes, so this returns as soon
// as a dialog opens.
func (p *Page) input(ctx context.Context, method string, params any) error {
	done := make(chan error, 1)
	go func() {
		_, err := p.t.Call(context.WithoutCancel(ctx), method, params)
		done <- err
	}()
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-tick.C:
			if p.Dialog() != nil {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Forget drops refs and the last snapshot (after the user used the page).
func (p *Page) Forget() {
	p.mu.Lock()
	p.newDocumentLocked()
	p.mu.Unlock()
}
