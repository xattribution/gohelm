// Package gohelm gives an assistant a browser and, in the Full profile,
// other Windows programs, as tools a model can call. See docs/design.md.
//
// A host makes one Session, takes its Tools, and maps each Tool to its own
// tool type. Each call gets a policy.Env: the host's Gate (how it asks the
// user), its Auditor, an optional Critic, and the owner (a chat) driving
// the browser.
package gohelm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/xattribution/gohelm/browser"
	"github.com/xattribution/gohelm/cdp"
	"github.com/xattribution/gohelm/desktop"
	"github.com/xattribution/gohelm/launch"
	"github.com/xattribution/gohelm/pane"
	"github.com/xattribution/gohelm/policy"
)

// Profile is how much the tools reach.
type Profile int

const (
	// Pane: a browser window the host owns, driven through WebView2's own
	// API. For locked-down computers and the host's main build.
	Pane Profile = iota
	// Full: the pane, an outside Edge or Chrome gohelm starts, and other
	// Windows programs. For development builds and personal use.
	Full
)

// PaneHandle is a browser window the host opened. *pane.Window is one.
type PaneHandle interface {
	Transport() cdp.Transport
	Show()
	SetStatus(string)
	Close() error
	Done() <-chan struct{}
}

// Config configures a Session.
type Config struct {
	Profile Profile
	// Rules are the site and action rules. For Quartermaster's main build
	// they come from the deployment file, with AskOutside off.
	Rules policy.Rules
	// Consent keeps "always" answers about sites, if the host offers them.
	Consent policy.Consent
	// PaneDir is the pane's own WebView2 profile folder, used when OpenPane
	// is nil (gohelm opens the pane itself).
	PaneDir string
	// PaneTitle is the pane's title ("Quartermaster browser").
	PaneTitle string
	// PaneOwner is the host's window, which the pane stays in front of.
	PaneOwner uintptr
	// OpenPane, if set, opens the host's own pane instead.
	OpenPane func(ctx context.Context, s *Session) (PaneHandle, error)
	// BrowserDir is the outside browser's own profile folder (Full only).
	BrowserDir string
	// BrowserExec is the outside browser's program; empty finds Edge, then
	// Chrome (Full only).
	BrowserExec string
	// BrowserHeadless runs the outside browser without a window (tests).
	BrowserHeadless bool
	// BrowserArgs are extra switches for the outside browser (tests).
	BrowserArgs []string
	// Vision offers screenshot tools, for hosts whose model takes pictures.
	Vision bool
	// HostPIDs are the host's processes besides this one, which the
	// desktop driver never touches.
	HostPIDs []int
	// Busy says the host is showing an approval card (desktop actions
	// wait).
	Busy func() bool
	// MaxChars and ReadChars cap snapshots and reading.
	MaxChars, ReadChars int
	// WorkingStatus is shown in the pane's title while a tool runs
	// (default "The assistant is working").
	WorkingStatus string
	// AgentName names the agent on the user's cards ("Quartermaster").
	// Default "the assistant".
	AgentName string
}

// Session holds the browser and desktop state the tools share.
type Session struct {
	cfg Config

	mu    sync.Mutex
	owner string
	pane  PaneHandle
	pPage *browser.Page
	ext   *launch.Browser
	tabs  []*tab
	cur   *tab // the outside browser's current tab
	where string
	desk  *desktop.Driver
}

type tab struct {
	id   string
	page *browser.Page
	t    cdp.Transport
}

// New makes a Session. Nothing opens until a tool needs it.
func New(cfg Config) (*Session, error) {
	if cfg.Profile == Pane && cfg.OpenPane == nil && cfg.PaneDir == "" {
		return nil, errors.New("gohelm: the pane needs a profile folder (PaneDir) or OpenPane")
	}
	if cfg.PaneTitle == "" {
		cfg.PaneTitle = "Browser"
	}
	if cfg.WorkingStatus == "" {
		cfg.WorkingStatus = "The assistant is working"
	}
	return &Session{cfg: cfg, where: "pane"}, nil
}

// Rules are the session's rules (hosts may change them between calls).
func (s *Session) Rules() *policy.Rules { return &s.cfg.Rules }

// claim lets one owner at a time use the session.
func (s *Session) claim(owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner == "" || s.owner == "" || s.owner == owner {
		if owner != "" {
			s.owner = owner
		}
		return nil
	}
	return errors.New("the browser is being used by another chat; finish or close it there first")
}

// Release lets another owner use the session.
func (s *Session) Release(owner string) {
	s.mu.Lock()
	if s.owner == owner {
		s.owner = ""
	}
	s.mu.Unlock()
}

// page returns the page the browser tools work on, opening the pane (or,
// in Full with where=browser, the outside browser) when needed.
func (s *Session) page(ctx context.Context, where string) (*browser.Page, error) {
	s.mu.Lock()
	if where == "" {
		where = s.where
	}
	if where == "browser" && s.cfg.Profile != Full {
		s.mu.Unlock()
		return nil, errors.New("only the built-in browser window is available here")
	}
	s.where = where
	s.mu.Unlock()
	if where == "browser" {
		return s.extPage(ctx)
	}
	return s.panePage(ctx)
}

func (s *Session) bopts() browser.Options {
	return browser.Options{Rules: &s.cfg.Rules, Consent: s.cfg.Consent, MaxChars: s.cfg.MaxChars, ReadChars: s.cfg.ReadChars, AgentName: s.cfg.AgentName}
}

func (s *Session) panePage(ctx context.Context) (*browser.Page, error) {
	s.mu.Lock()
	if s.pPage != nil {
		select {
		case <-s.pane.Done():
			s.pPage.Close()
			s.pane, s.pPage = nil, nil
		default:
			p, h := s.pPage, s.pane
			s.mu.Unlock()
			h.Show()
			return p, nil
		}
	}
	s.mu.Unlock()
	var h PaneHandle
	var err error
	if s.cfg.OpenPane != nil {
		h, err = s.cfg.OpenPane(ctx, s)
	} else {
		h, err = s.openPane(ctx)
	}
	if err != nil {
		return nil, err
	}
	p, err := browser.Attach(ctx, h.Transport(), s.bopts())
	if err != nil {
		h.Close()
		return nil, err
	}
	s.mu.Lock()
	s.pane, s.pPage = h, p
	s.mu.Unlock()
	h.Show()
	return p, nil
}

// openPane opens gohelm's own WebView2 window, wired to the rules.
func (s *Session) openPane(ctx context.Context) (PaneHandle, error) {
	w, err := pane.Open(ctx, pane.Options{
		Title:   s.cfg.PaneTitle,
		DataDir: s.cfg.PaneDir,
		Owner:   s.cfg.PaneOwner,
		AllowNavigation: func(u string, top, user bool) bool {
			// The host-level check: never a refused site, whoever navigates.
			// Asking about new sites happens in the page's own checks.
			return s.cfg.Rules.Check(u).Verdict != policy.Refused
		},
		Certificate: s.certificateOK,
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

// certificateOK lets a site ask for the user's certificate only when it's
// the page's own site and the agent may use it; Windows then asks the user
// for their PIN, which is the user's own yes.
func (s *Session) certificateOK(host string, port int) bool {
	s.mu.Lock()
	p := s.pPage
	s.mu.Unlock()
	if p == nil {
		return false
	}
	top := p.URL()
	v := s.cfg.Rules.Check(top)
	// The page got to its site with the rules' or the user's OK, so only a
	// request from another host (a frame, a sub-request) is refused.
	if v.Verdict == policy.Refused || !strings.EqualFold(v.Host, host) {
		return false
	}
	p.MarkCardSite(v.Origin)
	return true
}

func (s *Session) extPage(ctx context.Context) (*browser.Page, error) {
	s.mu.Lock()
	if s.cur != nil {
		p := s.cur.page
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()
	if err := s.startExt(ctx); err != nil {
		return nil, err
	}
	return s.newTab(ctx, "")
}

func (s *Session) startExt(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ext != nil {
		select {
		case <-s.ext.Exited():
			s.ext, s.tabs, s.cur = nil, nil, nil
		default:
			return nil
		}
	}
	if s.cfg.BrowserDir == "" {
		return errors.New("gohelm: the outside browser needs a profile folder (BrowserDir)")
	}
	b, err := launch.Start(ctx, launch.Options{Exec: s.cfg.BrowserExec, ProfileDir: filepath.Clean(s.cfg.BrowserDir), Headless: s.cfg.BrowserHeadless, Args: s.cfg.BrowserArgs})
	if err != nil {
		return err
	}
	s.ext = b
	return nil
}

func (s *Session) newTab(ctx context.Context, url string) (*browser.Page, error) {
	s.mu.Lock()
	b := s.ext
	s.mu.Unlock()
	if b == nil {
		return nil, errors.New("the outside browser isn't running")
	}
	id, err := b.NewPage(ctx, "about:blank")
	if err != nil {
		return nil, err
	}
	t, err := b.Attach(ctx, id)
	if err != nil {
		return nil, err
	}
	p, err := browser.Attach(ctx, t, s.bopts())
	if err != nil {
		b.ClosePage(ctx, id)
		return nil, err
	}
	tb := &tab{id: id, page: p, t: t}
	s.mu.Lock()
	s.tabs = append(s.tabs, tb)
	s.cur = tb
	s.mu.Unlock()
	return p, nil
}

func (s *Session) status(text string) {
	s.mu.Lock()
	h := s.pane
	where := s.where
	s.mu.Unlock()
	if h != nil && where == "pane" {
		h.SetStatus(text)
	}
}

// CloseBrowser closes the pane or the outside browser.
func (s *Session) CloseBrowser(where string) {
	s.mu.Lock()
	if where == "" {
		where = s.where
	}
	var h PaneHandle
	var p *browser.Page
	var ext *launch.Browser
	var tabs []*tab
	if where == "pane" {
		h, p = s.pane, s.pPage
		s.pane, s.pPage = nil, nil
	} else {
		ext, tabs = s.ext, s.tabs
		s.ext, s.tabs, s.cur = nil, nil, nil
	}
	s.owner = ""
	s.mu.Unlock()
	if p != nil {
		p.Close()
	}
	if h != nil {
		h.Close()
	}
	for _, t := range tabs {
		t.page.Close()
	}
	if ext != nil {
		ext.Close()
	}
}

// Close ends everything the session opened.
func (s *Session) Close() {
	s.CloseBrowser("pane")
	s.CloseBrowser("browser")
	s.mu.Lock()
	d := s.desk
	s.desk = nil
	s.mu.Unlock()
	if d != nil {
		d.Close()
	}
}

func (s *Session) desktop() (*desktop.Driver, error) {
	if s.cfg.Profile != Full {
		return nil, errors.New("other programs aren't available here")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.desk != nil {
		return s.desk, nil
	}
	d, err := desktop.New(desktop.Options{Rules: &s.cfg.Rules.Desktop, Confirm: &s.cfg.Rules, ExcludePIDs: s.cfg.HostPIDs, MaxChars: s.cfg.MaxChars, Busy: s.cfg.Busy, AgentName: s.cfg.AgentName})
	if err != nil {
		return nil, err
	}
	s.desk = d
	return d, nil
}

// tabsList describes the outside browser's tabs.
func (s *Session) tabsList(ctx context.Context) (string, error) {
	s.mu.Lock()
	b, cur := s.ext, s.cur
	s.mu.Unlock()
	if b == nil {
		return "The outside browser isn't open. browser_go with where=browser opens it.", nil
	}
	pages, err := b.Pages(ctx)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, p := range pages {
		mark := ""
		if cur != nil && cur.id == p.ID {
			mark = " (current)"
		}
		fmt.Fprintf(&sb, "- tab %d: %s — %s%s\n", i+1, policy.ShortPageText(p.Title, 80), policy.ShortPageText(p.URL, 120), mark)
	}
	if sb.Len() == 0 {
		return "No tabs are open.", nil
	}
	return sb.String(), nil
}
