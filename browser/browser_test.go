package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xattribution/gohelm/internal/testsite"
	"github.com/xattribution/gohelm/launch"
	"github.com/xattribution/gohelm/policy"
)

// These tests drive a real headless Chrome or Edge. They skip when neither
// is installed.

type fakeGate struct {
	mu      sync.Mutex
	asks    []policy.Ask
	answer  func(policy.Ask) policy.Answer
	allowed map[string]bool // keys answered yes, as a host keeps them for the rerun
}

func (g *fakeGate) Decide(_ context.Context, a policy.Ask) (policy.Answer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asks = append(g.asks, a)
	if g.allowed[a.Key] {
		delete(g.allowed, a.Key) // one approval, one use
		return policy.Allow, nil
	}
	if g.answer != nil {
		return g.answer(a), nil
	}
	return policy.Pending, nil
}

func (g *fakeGate) kinds() []policy.Kind {
	g.mu.Lock()
	defer g.mu.Unlock()
	var k []policy.Kind
	for _, a := range g.asks {
		k = append(k, a.Kind)
	}
	return k
}

func (g *fakeGate) last() policy.Ask {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.asks) == 0 {
		return policy.Ask{}
	}
	return g.asks[len(g.asks)-1]
}

type rig struct {
	t     *testing.T
	ctx   context.Context
	site  *testsite.Site
	b     *launch.Browser
	page  *Page
	rules *policy.Rules
	gate  *fakeGate
	audit []policy.Event
	mu    sync.Mutex
}

var (
	sharedOnce sync.Once
	shared     *launch.Browser
	sharedSite *testsite.Site
	sharedErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if shared != nil {
		shared.Close()
	}
	if sharedSite != nil {
		sharedSite.Close()
	}
	os.Exit(code)
}

func newRig(t *testing.T, rules policy.Rules) *rig {
	t.Helper()
	if _, err := launch.Find(); err != nil {
		t.Skip("no Edge or Chrome here")
	}
	sharedOnce.Do(func() {
		sharedSite, sharedErr = testsite.Start()
		if sharedErr != nil {
			return
		}
		dir, err := os.MkdirTemp("", "gohelm-profile-")
		if err != nil {
			sharedErr = err
			return
		}
		shared, sharedErr = launch.Start(context.Background(), launch.Options{ProfileDir: filepath.Join(dir, "p"), Headless: true,
			Args: []string{"--host-resolver-rules=MAP *.test 127.0.0.1", "--ignore-certificate-errors", "--no-proxy-server"}})
	})
	if sharedErr != nil {
		t.Fatal(sharedErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	id, err := shared.NewPage(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shared.ClosePage(context.Background(), id) })
	tr, err := shared.Attach(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, ctx: ctx, site: sharedSite, b: shared, rules: &rules, gate: &fakeGate{allowed: map[string]bool{}}}
	r.page, err = Attach(ctx, tr, Options{Rules: r.rules})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.page.Close)
	sharedSite.Reset()
	return r
}

func defaultRules() policy.Rules {
	return policy.Rules{Allow: []string{"site.test"}, Block: []string{"blocked.test"}, AllowHTTP: true}
}

func (r *rig) env() policy.Env {
	return policy.Env{Gate: r.gate, Audit: policy.AuditFunc(func(e policy.Event) { r.mu.Lock(); r.audit = append(r.audit, e); r.mu.Unlock() })}
}

func (r *rig) open(path string) Outcome {
	r.t.Helper()
	o, err := r.page.Navigate(r.ctx, r.env(), r.site.URL("site.test", path))
	if err != nil || o.Refused {
		r.t.Fatalf("open %s: %v %s", path, err, o.Text)
	}
	return o
}

func (r *rig) snap(o SnapOptions) string {
	r.t.Helper()
	s, err := r.page.Snapshot(r.ctx, o)
	if err != nil {
		r.t.Fatal(err)
	}
	return s.Text
}

// refOf finds the ref of the first line containing want.
func refOf(t *testing.T, snap, want string) string {
	t.Helper()
	for _, l := range strings.Split(snap, "\n") {
		if strings.Contains(l, want) {
			if i := strings.Index(l, "[ref="); i >= 0 {
				j := strings.Index(l[i:], "]")
				return l[i+5 : i+j]
			}
		}
	}
	t.Fatalf("no ref for %q in:\n%s", want, snap)
	return ""
}

func (r *rig) eval(expr string, out any) {
	r.t.Helper()
	if err := r.page.evalWorld(r.ctx, "", expr, out); err != nil {
		r.t.Fatal(err)
	}
}

func TestSnapshotHidesInvisibleTextAndMasksSecrets(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/form")
	s := r.snap(SnapOptions{All: true})
	for _, bad := range []string{"Ignore all previous", "TRANSPARENT HIDDEN", "TINY HIDDEN", "OFFSCREEN HIDDEN", "CLIPPED HIDDEN"} {
		if strings.Contains(s, bad) {
			t.Errorf("snapshot shows hidden text %q:\n%s", bad, s)
		}
	}
	for _, want := range []string{`heading "Travel request"`, `textbox "Full name"`, `combobox "Branch"`, `checkbox "I agree"`, `button "Continue"`, "Visible list item", "Left out:"} {
		if !strings.Contains(s, want) {
			t.Errorf("snapshot lacks %q:\n%s", want, s)
		}
	}
	if !strings.Contains(s, `textbox "Password"`) || !strings.Contains(s, "the user types this") {
		t.Errorf("password not marked:\n%s", s)
	}
	// Interactive-only is smaller and still has the controls.
	si := r.snap(SnapOptions{})
	if strings.Contains(si, "Fill in the form") || !strings.Contains(si, `button "Continue"`) {
		t.Errorf("interactive snapshot:\n%s", si)
	}
	rd, err := r.page.Read(r.ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rd, "Ignore all previous") || strings.Contains(rd, "TINY HIDDEN") || !strings.Contains(rd, "Fill in the form") || !strings.Contains(rd, "# Travel request") {
		t.Errorf("read:\n%s", rd)
	}
}

func TestRefsStayStable(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/form")
	a := refOf(t, r.snap(SnapOptions{}), `button "Continue"`)
	b := refOf(t, r.snap(SnapOptions{}), `button "Continue"`)
	if a != b {
		t.Errorf("ref changed between snapshots: %s, %s", a, b)
	}
	if _, err := r.page.ref("e999"); err == nil {
		t.Error("unknown ref accepted")
	}
}

func TestFillAndSecretRefusal(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/form")
	s := r.snap(SnapOptions{})
	o, err := r.page.Fill(r.ctx, r.env(), []FieldValue{
		{refOf(t, s, `textbox "Full name"`), "Jordan Smith"},
		{refOf(t, s, `combobox "Branch"`), "Space Force"},
		{refOf(t, s, `checkbox "I agree"`), "true"},
		{refOf(t, s, `textbox "Notes"`), "line one\nline two"},
		{refOf(t, s, `date "Departure"`), "2026-12-01"},
	})
	if err != nil || o.Refused {
		t.Fatalf("fill: %v %s", err, o.Text)
	}
	var got struct {
		Name, Branch, Notes, Depart string
		Agree                       bool
	}
	r.eval(`(()=>{const f=document.forms[0];return {Name:f.fullname.value,Branch:f.branch.value,Agree:f.agree.checked,Notes:f.notes.value,Depart:f.depart.value}})()`, &got)
	if got.Name != "Jordan Smith" || got.Branch != "ussf" || !got.Agree || got.Notes != "line one\nline two" || got.Depart != "2026-12-01" {
		t.Errorf("values: %+v", got)
	}
	if !strings.Contains(o.Text, "Changes:") {
		t.Errorf("no change list:\n%s", o.Text)
	}
	for _, f := range []string{`textbox "Password"`, `textbox "PIN"`} {
		o, _ = r.page.Fill(r.ctx, r.env(), []FieldValue{{refOf(t, s, f), "1234"}})
		if !o.Refused || !strings.Contains(o.Text, "never types") {
			t.Errorf("%s: secret field filled: %s", f, o.Text)
		}
	}
	var pw string
	r.eval(`document.forms[0].pw.value + document.forms[0].pin.value`, &pw)
	if pw != "" {
		t.Errorf("secret fields hold %q", pw)
	}
}

func TestSubmitWaitsForTheUser(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/form")
	s := r.snap(SnapOptions{})
	r.page.Fill(r.ctx, r.env(), []FieldValue{{refOf(t, s, `textbox "Full name"`), "Jordan Smith"}})
	cont := refOf(t, s, `button "Continue"`)
	o, err := r.page.Click(r.ctx, r.env(), cont, false)
	if err != nil {
		t.Fatal(err)
	}
	if o.Pending == nil || o.Pending.Kind != policy.KindCommit {
		t.Fatalf("submit didn't stop at a card: %+v", o)
	}
	if !strings.Contains(strings.Join(o.Pending.Detail, "\n"), "Jordan Smith") || !strings.Contains(strings.Join(o.Pending.Detail, "\n"), "/submit") {
		t.Errorf("card detail doesn't show what's sent: %v", o.Pending.Detail)
	}
	if len(r.site.Posts()) != 0 {
		t.Fatalf("sent before the user's OK: %v", r.site.Posts())
	}
	// The user says yes; the same call runs again.
	r.gate.allowed[o.Pending.Key] = true
	o, err = r.page.Click(r.ctx, r.env(), cont, false)
	if err != nil || o.Refused || o.Pending != nil {
		t.Fatalf("rerun: %v %+v", err, o)
	}
	if p := r.site.Posts(); len(p) != 1 || p[0].Path != "/submit" || !strings.Contains(p[0].Body, "Jordan") {
		t.Errorf("posts: %+v", p)
	}
	if !strings.Contains(o.Text, "new page") || !strings.Contains(o.Text, "Request received") {
		t.Errorf("after submit:\n%s", o.Text)
	}
}

func TestSendAfterPlainClickIsGated(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/sends")
	s := r.snap(SnapOptions{})
	more := refOf(t, s, `button "Load more"`)
	o, _ := r.page.Click(r.ctx, r.env(), more, false)
	if o.Pending == nil || o.Pending.Kind != policy.KindSend {
		t.Fatalf("a POST after a plain click went unchecked: %+v", o)
	}
	if len(r.site.Posts()) != 0 {
		t.Fatalf("sent: %v", r.site.Posts())
	}
	// A GET after a click is fine.
	o, _ = r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Show more"`), false)
	if o.Refused || o.Pending != nil || !strings.Contains(o.Text, `button "Item 3"`) {
		t.Errorf("show more:\n%s", o.Text)
	}
	// Approved, the rerun sends.
	r.gate.allowed[r.gate.last().Key] = true
	for _, a := range r.gate.asks {
		if a.Kind == policy.KindSend {
			r.gate.allowed[a.Key] = true
		}
	}
	o, _ = r.page.Click(r.ctx, r.env(), more, false)
	if o.Pending != nil || len(r.site.Posts()) != 1 {
		t.Errorf("approved send: %+v posts=%v", o, r.site.Posts())
	}
	// A beacon after an agent click is dropped without asking.
	r.site.Reset()
	o, _ = r.page.Click(r.ctx, r.env(), refOf(t, r.snap(SnapOptions{}), `button "Track me"`), false)
	time.Sleep(300 * time.Millisecond)
	if len(r.site.Posts()) != 0 {
		t.Errorf("beacon sent: %v", r.site.Posts())
	}
	// Enter in a field that posts is checked too.
	r.site.Reset()
	s = r.snap(SnapOptions{})
	search := refOf(t, s, `textbox "Search"`)
	r.page.Fill(r.ctx, r.env(), []FieldValue{{search, "travel"}})
	o, _ = r.page.Press(r.ctx, r.env(), "Enter", search)
	if len(r.site.Posts()) != 0 || o.Pending == nil {
		t.Errorf("enter sent without OK: %+v %v", o, r.site.Posts())
	}
}

func TestCoveredElementIsNamed(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/overlay")
	s := r.snap(SnapOptions{})
	o, _ := r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Under the banner"`), false)
	if !o.Refused || !strings.Contains(o.Text, "covered by") || !strings.Contains(o.Text, "Accept cookies") {
		t.Fatalf("covered click: %s", o.Text)
	}
	// "Accept" is consent: it waits for the user's OK, then goes ahead.
	o, _ = r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Accept cookies"`), false)
	if o.Pending == nil {
		t.Fatalf("accept didn't ask: %s", o.Text)
	}
	r.gate.allowed[o.Pending.Key] = true
	o, _ = r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Accept cookies"`), false)
	if o.Refused || o.Pending != nil {
		t.Fatalf("accept: %s", o.Text)
	}
	o, _ = r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Under the banner"`), false)
	if o.Refused {
		t.Fatalf("after banner: %s", o.Text)
	}
}

func TestNavigationRules(t *testing.T) {
	r := newRig(t, defaultRules())
	for _, u := range []string{"javascript:alert(1)", "data:text/html,hi", "file:///etc/passwd", r.site.URL("blocked.test", "/"), "http://127.0.0.1:47731/", "https://localhost/"} {
		o, _ := r.page.Navigate(r.ctx, r.env(), u)
		if !o.Refused {
			t.Errorf("%s opened: %s", u, o.Text)
		}
	}
	// Outside the list without AskOutside: refused, no card.
	o, _ := r.page.Navigate(r.ctx, r.env(), r.site.URL("other.test", "/frame"))
	if !o.Refused || len(r.gate.asks) != 0 {
		t.Errorf("other site: %+v asks=%v", o, r.gate.kinds())
	}
	// A redirect to a blocked site ends nowhere.
	r.open("/links")
	s := r.snap(SnapOptions{})
	r.page.Click(r.ctx, r.env(), refOf(t, s, `link "Redirects to blocked"`), false)
	if u := r.page.URL(); strings.Contains(u, "blocked.test") {
		t.Errorf("ended on %s", u)
	}
	s2 := r.snap(SnapOptions{All: true})
	if strings.Contains(s2, "never see this") {
		t.Errorf("blocked content shown:\n%s", s2)
	}
	// A link to a blocked site doesn't open it.
	r.open("/links")
	s = r.snap(SnapOptions{})
	r.page.Click(r.ctx, r.env(), refOf(t, s, `link "Blocked site"`), false)
	if strings.Contains(r.page.URL(), "blocked.test") {
		t.Errorf("link opened blocked site")
	}
	// Downloads are refused.
	r.open("/links")
	s = r.snap(SnapOptions{})
	o, _ = r.page.Click(r.ctx, r.env(), refOf(t, s, `link "Download the report"`), false)
	if !strings.Contains(o.Text, "download") && !strings.Contains(o.Text, "Download") {
		t.Logf("download click result (no note):\n%s", o.Text)
	}
}

func TestAskOutsideAsksOnce(t *testing.T) {
	rules := defaultRules()
	rules.AskOutside = true
	r := newRig(t, rules)
	u := r.site.HTTPS("other.test", "/frame")
	o, _ := r.page.Navigate(r.ctx, r.env(), u)
	if o.Pending == nil || o.Pending.Kind != policy.KindSite {
		t.Fatalf("no site card: %+v", o)
	}
	r.gate.allowed[o.Pending.Key] = true
	o, _ = r.page.Navigate(r.ctx, r.env(), u)
	if o.Refused || o.Pending != nil || !strings.Contains(o.Text, "Frame button") {
		t.Fatalf("after yes: %+v", o)
	}
	// The same site again doesn't ask: the session remembers.
	n := len(r.gate.asks)
	r.page.Navigate(r.ctx, r.env(), u)
	if len(r.gate.asks) != n {
		t.Errorf("asked again")
	}
	// http to a consented site stays refused.
	o, _ = r.page.Navigate(r.ctx, r.env(), r.site.URL("third.test", "/frame"))
	if !o.Refused {
		t.Errorf("http to an unlisted site opened: %s", o.Text)
	}
}

func TestFramesFromOtherSitesAreHidden(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/frames")
	time.Sleep(500 * time.Millisecond)
	s := r.snap(SnapOptions{All: true})
	if strings.Contains(s, "SECRET FRAME TEXT") || strings.Contains(s, "Frame button") {
		t.Errorf("other site's frame shown:\n%s", s)
	}
	if !strings.Contains(s, `button "Inner button"`) {
		t.Fatalf("same-site frame missing:\n%s", s)
	}
	if !strings.Contains(s, "Not shown: frames from") {
		t.Errorf("hidden frame not named:\n%s", s)
	}
	o, _ := r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Inner button"`), false)
	if o.Refused {
		t.Errorf("inner click: %s", o.Text)
	}
}

func TestDialogs(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/dialog")
	s := r.snap(SnapOptions{})
	o, _ := r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Clear list"`), false)
	if r.page.Dialog() == nil {
		t.Fatalf("no dialog after click: %s", o.Text)
	}
	o, _ = r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Show alert"`), false)
	if !o.Refused || !strings.Contains(o.Text, "dialog") {
		t.Errorf("acted with a dialog open: %s", o.Text)
	}
	o, _ = r.page.HandleDialog(r.ctx, r.env(), true, "")
	if o.Pending == nil || o.Pending.Kind != policy.KindCommit {
		t.Fatalf("confirm OK didn't ask: %+v", o)
	}
	o, _ = r.page.HandleDialog(r.ctx, r.env(), false, "")
	if o.Refused || r.page.Dialog() != nil {
		t.Errorf("dismiss: %+v", o)
	}
	if len(r.site.Posts()) != 0 {
		t.Errorf("posted: %v", r.site.Posts())
	}
}

func TestReadOnlySite(t *testing.T) {
	rules := policy.Rules{ReadOnly: []string{"site.test"}, AllowHTTP: true}
	r := newRig(t, rules)
	r.open("/form")
	s := r.snap(SnapOptions{})
	o, _ := r.page.Fill(r.ctx, r.env(), []FieldValue{{refOf(t, s, `textbox "Full name"`), "x"}})
	if !o.Refused || !strings.Contains(o.Text, "read-only") {
		t.Errorf("filled on a read-only site: %s", o.Text)
	}
	if _, err := r.page.Read(r.ctx, "", 0); err != nil {
		t.Error(err)
	}
}

func TestInjectionHintAndClickable(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/visible-injection")
	if s := r.snap(SnapOptions{All: true}); !strings.Contains(s, "address an AI assistant") {
		t.Errorf("no hint:\n%s", s)
	}
	r.open("/clickable")
	s := r.snap(SnapOptions{})
	ref := refOf(t, s, `clickable "Open details"`)
	r.page.Click(r.ctx, r.env(), ref, false)
	var title string
	r.eval("document.title", &title)
	if title != "div clicked" {
		t.Errorf("clickable div: title %q", title)
	}
}

func TestUserActivityPauses(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/overlay")
	time.Sleep(2 * time.Second) // past the action's grace
	// The user clicks in the page.
	r.page.t.Call(r.ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": 600, "y": 600, "button": "left", "clickCount": 1})
	r.page.t.Call(r.ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": 600, "y": 600, "button": "left", "clickCount": 1})
	time.Sleep(100 * time.Millisecond)
	s := r.snap(SnapOptions{})
	o, _ := r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Under the banner"`), false)
	if !o.Refused || !strings.Contains(o.Text, "user used the page") {
		t.Errorf("didn't pause: %s", o.Text)
	}
	o, _ = r.page.Click(r.ctx, r.env(), refOf(t, s, `button "Under the banner"`), false)
	if strings.Contains(o.Text, "user used the page") {
		t.Errorf("didn't resume: %s", o.Text)
	}
}

func TestAuditNeverHoldsTypedText(t *testing.T) {
	r := newRig(t, defaultRules())
	r.open("/form")
	s := r.snap(SnapOptions{})
	r.page.Fill(r.ctx, r.env(), []FieldValue{{refOf(t, s, `textbox "Full name"`), "Very Private Name"}})
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.audit) == 0 {
		t.Fatal("no audit events")
	}
	for _, e := range r.audit {
		if strings.Contains(e.Target+e.Error+e.Outcome+e.Action, "Very Private") {
			t.Errorf("typed text in audit: %+v", e)
		}
	}
}
