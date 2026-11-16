// Package browser drives one page over a DevTools transport: it describes
// the page as a snapshot with refs, reads it, and clicks, fills and presses
// keys with the checks in package policy. The same code drives the WebView2
// pane and an outside browser.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xattribution/gohelm/cdp"
	"github.com/xattribution/gohelm/policy"
)

// Options configure a Page.
type Options struct {
	// Rules are the site and action rules (required).
	Rules *policy.Rules
	// Consent, if set, keeps "this site" answers beyond the session.
	Consent policy.Consent
	// MaxChars caps a snapshot's text (default 6000).
	MaxChars int
	// ReadChars caps one browser_read result (default 8000).
	ReadChars int
	// OnUserActivity, if set, is called when the user uses the page
	// themselves while the agent is working with it.
	OnUserActivity func()
}

// Page is one page the agent works with.
type Page struct {
	t    cdp.Transport
	o    Options
	subs []func()

	mu         sync.Mutex
	mainFrame  string
	loaderID   string
	url, title string
	gen        int
	refs       map[string]refTarget // "e12" → node
	byNode     map[int]string       // backend node ID → ref
	nextRef    int
	lastSnap   *Snapshot
	dialog     *Dialog
	worlds     map[string]int // frame ID → gohelm's isolated world
	grants     map[string]policy.Grant
	cardSites  map[string]bool
	sendOK     map[string]bool // origins whose sends the user allowed for the session
	filled     map[string]string
	loading    bool
	loadedAt   time.Time
	notes      []string

	// While the agent acts: the call's Env, and what the page tried.
	acting        bool
	actEnv        policy.Env
	actCtx        context.Context
	actTarget     string
	actEnd        time.Time
	allowSendTill time.Time
	approvedNext  bool // the next action was approved as a commit
	blocked       []blockedReq
	pending       *policy.Ask
	cannotAsk     string
	inflight      int       // questions the page's requests are waiting on
	userInputAt   time.Time // last real input the page saw (from gohelm's world)
	closed        bool
}

type refTarget struct {
	backend int
	frame   string
	gen     int
	role    string
	name    string
}

// Dialog is an open alert, confirm, prompt or leave-page dialog.
type Dialog struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Default string `json:"defaultPrompt"`
}

type blockedReq struct {
	method, url, kind string
}

// Attach starts working with the page behind t. It turns on the DevTools
// domains gohelm uses and its request checks.
func Attach(ctx context.Context, t cdp.Transport, o Options) (*Page, error) {
	if o.Rules == nil {
		return nil, errors.New("browser: rules are required")
	}
	if o.MaxChars <= 0 {
		o.MaxChars = 6000
	}
	if o.ReadChars <= 0 {
		o.ReadChars = 8000
	}
	p := &Page{t: t, o: o, refs: map[string]refTarget{}, byNode: map[int]string{}, worlds: map[string]int{},
		grants: map[string]policy.Grant{}, cardSites: map[string]bool{}, sendOK: map[string]bool{}, filled: map[string]string{}}
	p.listen()
	for _, m := range []struct {
		method string
		params any
	}{
		{"Page.enable", nil},
		{"Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}},
		{"Runtime.enable", nil},
		{"DOM.enable", nil},
		{"Accessibility.enable", nil},
		{"Page.setInterceptFileChooserDialog", map[string]any{"enabled": true}},
		{"Fetch.enable", map[string]any{"handleAuthRequests": true, "patterns": fetchPatterns()}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": worldScript, "worldName": worldName, "runImmediately": true}},
	} {
		_, err := t.Call(ctx, m.method, m.params)
		if err != nil && m.method == "Fetch.enable" {
			// An older engine that doesn't know a kind in the list: check
			// every request instead.
			_, err = t.Call(ctx, m.method, map[string]any{"handleAuthRequests": true, "patterns": []map[string]any{{"urlPattern": "*", "requestStage": "Request"}}})
		}
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("browser: %s: %w", m.method, err)
		}
	}
	// Downloads are refused in this version (the pane refuses them too).
	t.Call(ctx, "Page.setDownloadBehavior", map[string]any{"behavior": "deny"})
	if err := p.refreshFrame(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func fetchPatterns() []map[string]any {
	// Only the kinds of request that can send something: documents (form
	// posts and navigation), XHR and fetch, pings and beacons, and the rest.
	var out []map[string]any
	for _, k := range []string{"Document", "XHR", "Fetch", "Ping", "CSPViolationReport", "Other", "EventSource"} {
		out = append(out, map[string]any{"urlPattern": "*", "resourceType": k, "requestStage": "Request"})
	}
	return out
}

const worldName = "gohelm"

// worldScript runs in gohelm's isolated world in every document: it counts
// DOM changes (to tell when a page settles) and notes the time of real
// input. The page's own scripts can't see or change it.
const worldScript = `(() => {
  if (window.__gohelm) return;
  const s = {m: 0, last: 0, input: 0};
  Object.defineProperty(window, '__gohelm', {value: s});
  try {
    new MutationObserver(r => { s.m += r.length; s.last = Date.now(); })
      .observe(document, {subtree: true, childList: true, attributes: true, characterData: true});
  } catch (e) {}
  for (const ev of ['pointerdown', 'keydown', 'wheel']) {
    addEventListener(ev, e => { if (e.isTrusted) s.input = Date.now(); }, true);
  }
})()`

func (p *Page) listen() {
	on := func(ev string, fn func(json.RawMessage)) { p.subs = append(p.subs, p.t.Subscribe(ev, fn)) }
	on("Page.frameNavigated", func(raw json.RawMessage) {
		var e struct {
			Frame struct {
				ID       string `json:"id"`
				ParentID string `json:"parentId"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
				Fragment string `json:"urlFragment"`
			} `json:"frame"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Frame.ParentID != "" {
			return
		}
		p.mu.Lock()
		p.mainFrame = e.Frame.ID
		u := e.Frame.URL + e.Frame.Fragment
		if e.Frame.LoaderID != p.loaderID {
			p.loaderID = e.Frame.LoaderID
			p.newDocumentLocked()
		}
		p.url = u
		p.mu.Unlock()
		// A page that ends up somewhere it may not be (a redirect the checks
		// missed, a service worker, the back-forward cache) is left blank.
		if v := p.o.Rules.Check(u); v.Verdict == policy.Refused && u != "about:blank" {
			go p.t.Call(context.Background(), "Page.navigate", map[string]any{"url": "about:blank"})
			p.note("Left " + v.Host + ": " + v.Reason + ".")
		}
	})
	on("Page.navigatedWithinDocument", func(raw json.RawMessage) {
		var e struct {
			FrameID string `json:"frameId"`
			URL     string `json:"url"`
		}
		if json.Unmarshal(raw, &e) == nil {
			p.mu.Lock()
			if e.FrameID == p.mainFrame {
				p.url = e.URL
			}
			p.mu.Unlock()
		}
	})
	on("Page.frameStartedLoading", func(raw json.RawMessage) {
		var e struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(raw, &e) == nil {
			p.mu.Lock()
			if e.FrameID == p.mainFrame || p.mainFrame == "" {
				p.loading = true
			}
			p.mu.Unlock()
		}
	})
	on("Page.frameStoppedLoading", func(raw json.RawMessage) {
		var e struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(raw, &e) == nil {
			p.mu.Lock()
			if e.FrameID == p.mainFrame {
				p.loading = false
				p.loadedAt = time.Now()
			}
			p.mu.Unlock()
		}
	})
	on("Page.javascriptDialogOpening", func(raw json.RawMessage) {
		var d Dialog
		if json.Unmarshal(raw, &d) == nil {
			p.mu.Lock()
			p.dialog = &d
			p.mu.Unlock()
		}
	})
	on("Page.javascriptDialogClosed", func(json.RawMessage) {
		p.mu.Lock()
		p.dialog = nil
		p.mu.Unlock()
	})
	on("Page.fileChooserOpened", func(json.RawMessage) {
		p.note("The page asked for a file to upload. gohelm doesn't upload files; the user can do it themselves (browser_hand_over).")
	})
	on("Page.downloadWillBegin", func(raw json.RawMessage) {
		var e struct {
			URL string `json:"url"`
		}
		json.Unmarshal(raw, &e)
		p.note("The page started a download, and it was refused: gohelm doesn't download files.")
	})
	on("Page.windowOpen", func(raw json.RawMessage) {
		var e struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &e) == nil {
			p.note("The page tried to open a new window at " + shortURL(e.URL) + ".")
		}
	})
	on("Runtime.executionContextCreated", func(raw json.RawMessage) {
		var e struct {
			Context struct {
				ID      int    `json:"id"`
				Name    string `json:"name"`
				AuxData struct {
					FrameID   string `json:"frameId"`
					IsDefault bool   `json:"isDefault"`
				} `json:"auxData"`
			} `json:"context"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Context.Name == worldName && !e.Context.AuxData.IsDefault {
			p.mu.Lock()
			p.worlds[e.Context.AuxData.FrameID] = e.Context.ID
			p.mu.Unlock()
		}
	})
	on("Runtime.executionContextDestroyed", func(raw json.RawMessage) {
		var e struct {
			ID int `json:"executionContextId"`
		}
		if json.Unmarshal(raw, &e) == nil {
			p.mu.Lock()
			for f, id := range p.worlds {
				if id == e.ID {
					delete(p.worlds, f)
				}
			}
			p.mu.Unlock()
		}
	})
	on("Runtime.executionContextsCleared", func(json.RawMessage) {
		p.mu.Lock()
		p.worlds = map[string]int{}
		p.mu.Unlock()
	})
	on("Fetch.requestPaused", p.onRequest)
	on("Fetch.authRequired", func(raw json.RawMessage) {
		var e struct {
			RequestID string `json:"requestId"`
			Request   struct {
				URL string `json:"url"`
			} `json:"request"`
		}
		if json.Unmarshal(raw, &e) != nil {
			return
		}
		p.t.Call(context.Background(), "Fetch.continueWithAuth", map[string]any{"requestId": e.RequestID, "authChallengeResponse": map[string]any{"response": "CancelAuth"}})
		p.note(hostOf(e.Request.URL) + " asked for a user name and password; gohelm never types them. The user can sign in themselves (browser_hand_over).")
	})
}

// newDocumentLocked forgets what belonged to the old document.
func (p *Page) newDocumentLocked() {
	p.gen++
	p.refs = map[string]refTarget{}
	p.byNode = map[int]string{}
	p.nextRef = 0
	p.lastSnap = nil
	p.filled = map[string]string{}
	p.dialog = nil
}

func (p *Page) note(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range p.notes {
		if n == s {
			return
		}
	}
	if len(p.notes) < 10 {
		p.notes = append(p.notes, s)
	}
}

func (p *Page) takeNotes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.notes
	p.notes = nil
	return n
}

// refreshFrame reads the main frame's ID, address and loader.
func (p *Page) refreshFrame(ctx context.Context) error {
	var r struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := cdp.Do(ctx, p.t, "Page.getFrameTree", nil, &r); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	f := r.FrameTree.Frame
	p.mainFrame = f.ID
	if f.LoaderID != p.loaderID {
		p.loaderID = f.LoaderID
		p.newDocumentLocked()
	}
	p.url = f.URL
	return nil
}

// URL is the page's address.
func (p *Page) URL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url
}

// Origin is the page's origin, from its address.
func (p *Page) Origin() string { return originOf(p.URL()) }

// Close stops listening. It doesn't close the page itself.
func (p *Page) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	for _, c := range p.subs {
		c()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.t.Call(ctx, "Fetch.disable", nil)
}

// MarkCardSite records that origin asked for the user's certificate, so
// its Confirm level applies (the pane calls it).
func (p *Page) MarkCardSite(origin string) {
	p.mu.Lock()
	p.cardSites[origin] = true
	p.mu.Unlock()
}

// grant is what the agent may do on origin: the rules, then this session's
// and the host's remembered answers.
func (p *Page) grant(raw string) (policy.SiteResult, policy.Grant) {
	v := p.o.Rules.Check(raw)
	switch v.Verdict {
	case policy.Refused:
		return v, policy.GrantDenied
	case policy.Allowed:
		return v, policy.GrantAct
	}
	p.mu.Lock()
	g := p.grants[v.Origin]
	p.mu.Unlock()
	if g == policy.NoGrant && p.o.Consent != nil {
		g = p.o.Consent.Site(v.Origin)
	}
	if v.Verdict == policy.ReadOnly {
		if g == policy.GrantDenied {
			return v, g
		}
		return v, policy.GrantRead
	}
	return v, g
}

func (p *Page) setGrant(origin string, g policy.Grant, always bool) {
	p.mu.Lock()
	p.grants[origin] = g
	p.mu.Unlock()
	if always && p.o.Consent != nil {
		p.o.Consent.SetSite(origin, g)
	}
}

// onRequest checks each request the page makes that could send something
// or open a document.
func (p *Page) onRequest(raw json.RawMessage) {
	var e struct {
		RequestID string `json:"requestId"`
		FrameID   string `json:"frameId"`
		Type      string `json:"resourceType"`
		Request   struct {
			URL    string `json:"url"`
			Method string `json:"method"`
		} `json:"request"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	cont := func() {
		p.t.Call(context.Background(), "Fetch.continueRequest", map[string]any{"requestId": e.RequestID})
	}
	fail := func() {
		p.t.Call(context.Background(), "Fetch.failRequest", map[string]any{"requestId": e.RequestID, "errorReason": "BlockedByClient"})
	}
	method := strings.ToUpper(e.Request.Method)
	p.mu.Lock()
	acting := p.acting || time.Now().Before(p.actEnd)
	isMain := e.FrameID == p.mainFrame || p.mainFrame == ""
	env, actx, target := p.actEnv, p.actCtx, p.actTarget
	sendAllowed := time.Now().Before(p.allowSendTill)
	p.mu.Unlock()

	if e.Type == "Document" {
		v, g := p.grant(e.Request.URL)
		switch {
		case v.Verdict == policy.Refused:
			fail()
			if isMain {
				p.note("Didn't open " + shortURL(e.Request.URL) + ": " + v.Reason + ".")
			}
			return
		case isMain && g == policy.GrantDenied:
			fail()
			p.note("Didn't open " + v.Host + ": the user said no to it.")
			return
		case isMain && g == policy.NoGrant && v.Verdict == policy.AskUser:
			if !acting {
				// The user went there themselves: their choice, for reading.
				p.setGrant(v.Origin, policy.GrantRead, false)
				break
			}
			ask := policy.Ask{Kind: policy.KindSite, Key: policy.MakeKey(policy.KindSite, v.Origin), Origin: v.Origin, URL: e.Request.URL,
				Title: "Let the assistant open " + v.Host + "?", Detail: []string{"Address: " + e.Request.URL},
				Options: []policy.Option{policy.OptAllow, policy.OptDeny}}
			p.addInflight(1)
			go func() {
				defer p.addInflight(-1)
				a, err := env.Ask(actx, ask)
				switch {
				case err == nil && (a == policy.Allow || a == policy.AllowSite):
					p.setGrant(v.Origin, policy.GrantAct, a == policy.AllowSite)
					cont()
				default:
					fail()
					p.holdAnswer(ask, a, err)
				}
			}()
			return
		}
	}
	if method == "GET" || method == "HEAD" || method == "OPTIONS" {
		cont()
		return
	}
	// The page wants to send something.
	if e.Type == "Ping" || e.Type == "CSPViolationReport" {
		if acting {
			fail() // beacons wait for nobody
			return
		}
		cont()
		return
	}
	origin := originOf(e.Request.URL)
	p.mu.Lock()
	siteOK := p.sendOK[originOf(p.url)] || p.sendOK[origin]
	p.mu.Unlock()
	if !acting || sendAllowed || siteOK {
		cont()
		return
	}
	// It followed an agent action the user didn't approve as a send.
	ask := policy.Ask{Kind: policy.KindSend, Key: policy.MakeKey(policy.KindSend, originOf(p.URL()), target, method, origin+pathOf(e.Request.URL), p.filledDigest()),
		Origin: origin, URL: e.Request.URL,
		Title:   fmt.Sprintf("Let the page send data to %s after %s?", hostOf(e.Request.URL), target),
		Detail:  append([]string{"Sends to: " + shortURL(e.Request.URL) + " (" + method + ")"}, p.filledLines()...),
		Options: []policy.Option{policy.OptAllow, policy.OptAllowSite, policy.OptDeny}}
	p.addInflight(1)
	go func() {
		defer p.addInflight(-1)
		a, err := env.Ask(actx, ask)
		switch {
		case err == nil && a == policy.Allow:
			p.mu.Lock()
			p.allowSendTill = time.Now().Add(15 * time.Second)
			p.mu.Unlock()
			cont()
		case err == nil && a == policy.AllowSite:
			p.mu.Lock()
			p.sendOK[originOf(p.url)] = true
			p.mu.Unlock()
			cont()
		default:
			fail()
			p.mu.Lock()
			p.blocked = append(p.blocked, blockedReq{method, e.Request.URL, e.Type})
			p.mu.Unlock()
			p.holdAnswer(ask, a, err)
		}
	}()
}

func (p *Page) addInflight(n int) {
	p.mu.Lock()
	p.inflight += n
	p.mu.Unlock()
}

// holdAnswer keeps a question that wasn't answered yes, for the action's
// result.
func (p *Page) holdAnswer(a policy.Ask, ans policy.Answer, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case err != nil:
		p.cannotAsk = err.Error()
	case ans == policy.Pending:
		if p.pending == nil {
			p.pending = &a
		}
	case ans == policy.CannotAsk:
		p.cannotAsk = "nobody can answer the question in this run: " + a.Title
	default:
		p.cannotAsk = "the user said no: " + a.Title
	}
}

func (p *Page) filledLines() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for k, v := range p.filled {
		out = append(out, k+" = "+policy.ShortPageText(v, 60))
	}
	sortStrings(out)
	if len(out) > 8 {
		out = append(out[:8], fmt.Sprintf("…and %d more fields", len(out)-8))
	}
	if len(out) > 0 {
		out = append([]string{"Filled in by the assistant on this page:"}, out...)
	}
	return out
}

func (p *Page) filledDigest() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var parts []string
	for k, v := range p.filled {
		parts = append(parts, k+"="+v)
	}
	sortStrings(parts)
	return strings.Join(parts, "\x00")
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Hostname()
}

func pathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

// shortURL is an address cut for a message: host and path, query dropped
// when long.
func shortURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return policy.ShortPageText(raw, 120)
	}
	s := u.Scheme + "://" + u.Host + u.Path
	if u.RawQuery != "" {
		if len(u.RawQuery) <= 60 {
			s += "?" + u.RawQuery
		} else {
			s += "?…"
		}
	}
	return policy.ShortPageText(s, 160)
}
