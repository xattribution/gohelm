// Package policy decides what the agent may do: which sites it may read
// and act on, which actions need the user's OK, and which it never does.
// It defines the Gate, Critic, Consent and Auditor interfaces a host
// implements; gohelm itself keeps no files.
package policy

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// Rules are the site and action rules. A host fills them from its
// deployment configuration; the zero value allows nothing but asking.
type Rules struct {
	// Allow lists sites the agent may read and act on without asking.
	// "example.mil" covers that host and its subdomains (as Edge's URL
	// policies do), ".example.mil" that host only, "*" every site. A
	// scheme ("https://"), port or path may be given; paths are ignored.
	Allow []string `json:"allow,omitempty"`
	// ReadOnly lists sites the agent may read but never act on.
	ReadOnly []string `json:"read_only,omitempty"`
	// Block lists sites the agent may never open. Block wins over Allow.
	Block []string `json:"block,omitempty"`
	// AskOutside lets the agent ask the user about a site on no list. Off,
	// sites on no list are refused. Quartermaster's main build keeps it
	// off: it talks only to hosts an administrator named.
	AskOutside bool `json:"ask_outside,omitempty"`
	// AllowHTTP allows plain http:// to sites on the Allow list (never to
	// sites the user only consented to).
	AllowHTTP bool `json:"allow_http,omitempty"`
	// AllowPrivate allows private-network and single-label host names
	// (intranet sites) when they are on the Allow list. Loopback
	// addresses and localhost are never allowed.
	AllowPrivate bool `json:"allow_private,omitempty"`
	// Confirm is which actions wait for the user: "commit" (default:
	// sending, submitting, buying, deleting, signing) or "input" (also
	// typing and choosing).
	Confirm string `json:"confirm,omitempty"`
	// CardSitesConfirm is Confirm on sites that asked for the user's
	// certificate (CAC). Empty means the same as Confirm.
	CardSitesConfirm string `json:"card_sites_confirm,omitempty"`
	// Desktop are the desktop driver's rules (Full profile only).
	Desktop DesktopRules `json:"desktop,omitempty"`
}

// DesktopRules limit the desktop driver.
type DesktopRules struct {
	// Apps lists the programs (by file name, "excel.exe") the agent may
	// read and act on. Empty means none.
	Apps []string `json:"apps,omitempty"`
	// RawInput allows synthetic mouse and keyboard input when an element
	// has no UI Automation pattern for the action. Off by default.
	RawInput bool `json:"raw_input,omitempty"`
}

// Verdict is what the rules say about a site.
type Verdict int

const (
	// Refused: never open it.
	Refused Verdict = iota
	// AskUser: open only after the user agrees.
	AskUser
	// ReadOnly: open and read; never act.
	ReadOnly
	// Allowed: read and act.
	Allowed
)

func (v Verdict) String() string {
	return [...]string{"refused", "ask", "read-only", "allowed"}[v]
}

// SiteResult is the verdict and, when refused, why.
type SiteResult struct {
	Verdict Verdict
	Origin  string // scheme://host[:port]
	Host    string
	Reason  string
}

// ErrBadURL is a URL gohelm never opens.
var ErrBadURL = errors.New("policy: not a web address gohelm opens")

// Check says whether the agent may open u.
func (r *Rules) Check(raw string) SiteResult {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" && u.Scheme != "about" {
		return SiteResult{Verdict: Refused, Reason: "it isn't a complete web address"}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "about" {
		if strings.EqualFold(u.Opaque, "blank") || strings.EqualFold(raw, "about:blank") {
			return SiteResult{Verdict: Allowed, Origin: "about:blank"}
		}
		return SiteResult{Verdict: Refused, Reason: "only about:blank is opened"}
	}
	if scheme != "https" && scheme != "http" {
		return SiteResult{Verdict: Refused, Reason: scheme + ": addresses are never opened"}
	}
	if u.User != nil {
		return SiteResult{Verdict: Refused, Reason: "addresses with a user name or password in them are never opened"}
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	origin := scheme + "://" + host
	if p := u.Port(); p != "" {
		origin += ":" + p
	}
	res := SiteResult{Origin: origin, Host: host}
	if host == "" {
		res.Reason = "it has no host"
		return res
	}
	if isLoopback(host) {
		res.Reason = "this computer's own addresses are never opened"
		return res
	}
	if matchAny(r.Block, scheme, host, u.Port()) {
		res.Reason = "the policy blocks this site"
		return res
	}
	allowed := matchAny(r.Allow, scheme, host, u.Port())
	readOnly := matchAny(r.ReadOnly, scheme, host, u.Port())
	if isPrivate(host) && !(r.AllowPrivate && (allowed || readOnly)) {
		res.Reason = "private-network addresses are opened only when the policy lists them"
		return res
	}
	if scheme == "http" && !(r.AllowHTTP && (allowed || readOnly)) {
		res.Reason = "plain http:// is opened only for sites the policy lists"
		return res
	}
	switch {
	case allowed:
		res.Verdict = Allowed
	case readOnly:
		res.Verdict = ReadOnly
	case r.AskOutside:
		res.Verdict = AskUser
	default:
		res.Reason = "the policy doesn't list this site"
	}
	return res
}

func matchAny(patterns []string, scheme, host, port string) bool {
	for _, p := range patterns {
		if matchSite(p, scheme, host, port) {
			return true
		}
	}
	return false
}

// matchSite matches one pattern: [scheme://]host[:port][/path].
func matchSite(p, scheme, host, port string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return false
	}
	if p == "*" {
		return true
	}
	if i := strings.Index(p, "://"); i >= 0 {
		if p[:i] != scheme {
			return false
		}
		p = p[i+3:]
	}
	if i := strings.IndexAny(p, "/?#"); i >= 0 {
		p = p[:i]
	}
	if h, pt, err := net.SplitHostPort(p); err == nil {
		if pt != port {
			return false
		}
		p = h
	}
	p = strings.TrimSuffix(p, ".")
	switch {
	case strings.HasPrefix(p, "*."):
		d := p[2:]
		return host == d || strings.HasSuffix(host, "."+d)
	case strings.HasPrefix(p, "."):
		return host == p[1:]
	default:
		return host == p || strings.HasSuffix(host, "."+p)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsUnspecified()
}

// isPrivate is a private, link-local or shared address, or a single-label
// host name (which only resolves inside a network).
func isPrivate(host string) bool {
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return !strings.Contains(host, ".")
	}
	a = a.Unmap()
	if a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() {
		return true
	}
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	return cgnat.Contains(a)
}

// confirmLevel is the Confirm level for a site.
func (r *Rules) confirmLevel(cardSite bool) string {
	c := r.Confirm
	if cardSite && r.CardSitesConfirm != "" {
		c = r.CardSitesConfirm
	}
	if c != "input" {
		c = "commit"
	}
	return c
}

// NeedsOK says whether an action of this class waits for the user.
// Secret actions are never done at all; callers check that first.
func (r *Rules) NeedsOK(c Class, cardSite bool) bool {
	switch c {
	case Commit:
		return true
	case Input:
		return r.confirmLevel(cardSite) == "input"
	}
	return false
}

// AppAllowed says whether the desktop driver may touch a program.
func (d *DesktopRules) AppAllowed(exe string) bool {
	exe = strings.ToLower(exe)
	if i := strings.LastIndexAny(exe, `\/`); i >= 0 {
		exe = exe[i+1:]
	}
	for _, a := range d.Apps {
		if strings.EqualFold(strings.TrimSpace(a), exe) {
			return true
		}
	}
	return false
}
