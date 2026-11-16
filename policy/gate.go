package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Kind is what an Ask is about.
type Kind string

const (
	// KindSite: open a site on no list.
	KindSite Kind = "site"
	// KindCommit: an action that sends or changes something.
	KindCommit Kind = "commit"
	// KindInput: typing or choosing, when the rules confirm input.
	KindInput Kind = "input"
	// KindSend: the page tried to send data after an action that wasn't
	// approved as a commit.
	KindSend Kind = "send"
	// KindCertificate: a site asks for the user's certificate (CAC).
	KindCertificate Kind = "certificate"
	// KindHandOver: the agent asks the user to do something themselves.
	KindHandOver Kind = "hand_over"
	// KindDesktop: an action in another program (Full profile).
	KindDesktop Kind = "desktop"
)

// Option is a button on the host's card.
type Option string

const (
	OptAllow     Option = "allow"      // this once
	OptAllowSite Option = "allow_site" // this site, for the rest of the session or always (the host decides)
	OptDeny      Option = "deny"
	OptDone      Option = "done" // hand-over finished
)

// Ask is a question for the user. It is plain data, so a host can keep it
// with a card and match the answer to the call when it runs again.
type Ask struct {
	Kind Kind `json:"kind"`
	// Key is stable for the same question about the same thing, so the
	// call run again after the user's answer finds that answer. One
	// approval answers one Key once.
	Key string `json:"key"`
	// Origin and URL are the site's.
	Origin string `json:"origin,omitempty"`
	URL    string `json:"url,omitempty"`
	// Title is one plain sentence for the card: "Click "Submit request" on
	// travel.example.mil?"
	Title string `json:"title"`
	// Detail lines: what was filled in, where it goes.
	Detail []string `json:"detail,omitempty"`
	// Options are the card's buttons, in order.
	Options []Option `json:"options"`
}

// Answer is the user's answer, or why there is none yet.
type Answer int

const (
	// Deny: no.
	Deny Answer = iota
	// Allow: yes, this once.
	Allow
	// AllowSite: yes, and the same for this site.
	AllowSite
	// Done: the user finished what the hand-over asked.
	Done
	// Pending: the host is showing a card; stop and run the call again
	// after the user answers.
	Pending
	// CannotAsk: nobody can answer in this run (a scheduled run, a helper).
	CannotAsk
)

func (a Answer) String() string {
	return [...]string{"deny", "allow", "allow_site", "done", "pending", "cannot_ask"}[a]
}

// Gate asks the user. A host's Gate either blocks until the user answers
// or shows a card and returns Pending. It is asked before anything the
// question is about happens.
type Gate interface {
	Decide(ctx context.Context, a Ask) (Answer, error)
}

// GateFunc adapts a function to Gate.
type GateFunc func(ctx context.Context, a Ask) (Answer, error)

// Decide calls f.
func (f GateFunc) Decide(ctx context.Context, a Ask) (Answer, error) { return f(ctx, a) }

// NoGate answers CannotAsk to everything: nothing that needs the user
// happens.
var NoGate Gate = GateFunc(func(context.Context, Ask) (Answer, error) { return CannotAsk, nil })

// MakeKey builds an Ask's Key from the parts that identify the question.
func MakeKey(kind Kind, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return string(kind) + ":" + hex.EncodeToString(h.Sum(nil))[:24]
}

// Proposal is what the Critic sees: the user's task as the host states it
// and the action's metadata. Role and name come from the page, so they are
// cut short and marked as page text.
type Proposal struct {
	Task   string `json:"task"`
	Class  Class  `json:"class"`
	Action string `json:"action"` // click, fill, press, open
	Origin string `json:"origin"`
	Role   string `json:"role,omitempty"`
	Name   string `json:"name,omitempty"` // page text, at most 80 characters
}

// Critic is an optional second check of commit actions. It never sees
// page content beyond the target's role and short name.
type Critic interface {
	Review(ctx context.Context, p Proposal) (ok bool, reason string, err error)
}

// ShortPageText cuts page text for a Proposal or a card.
func ShortPageText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Grant is a remembered decision about a site.
type Grant int

const (
	NoGrant Grant = iota
	GrantRead
	GrantAct
	GrantDenied
)

// Consent remembers "this site" answers. gohelm keeps answers for the
// session in memory; a host that offers "always" stores them here.
type Consent interface {
	Site(origin string) Grant
	SetSite(origin string, g Grant)
}

// Event is one line of the audit trail. Typed text is never in it, only
// its length.
type Event struct {
	Time     time.Time `json:"time"`
	Phase    string    `json:"phase"` // intent | result
	Tool     string    `json:"tool"`
	Action   string    `json:"action"`
	Class    string    `json:"class,omitempty"`
	Origin   string    `json:"origin,omitempty"`
	Target   string    `json:"target,omitempty"` // role "name", cut short
	Decision string    `json:"decision,omitempty"`
	Outcome  string    `json:"outcome,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Auditor receives every audit event, synchronously: intent before an
// action, result after.
type Auditor interface {
	Record(e Event)
}

// AuditFunc adapts a function to Auditor.
type AuditFunc func(Event)

// Record calls f.
func (f AuditFunc) Record(e Event) { f(e) }
