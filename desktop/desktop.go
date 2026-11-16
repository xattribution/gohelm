// Package desktop reads and operates other Windows programs for gohelm's
// Full profile, through UI Automation: the accessibility tree that screen
// readers use. Actions use UI Automation patterns (Invoke, Toggle, Value,
// Select, Expand/Collapse) and only fall back to synthetic mouse and
// keyboard input when the rules allow it and the target is in front.
//
// It touches only programs on the rules' list. The host's own windows are
// always off limits, so the agent can never click its own approval
// buttons, and so are Windows Security, User Account Control, credential
// and smart card PIN prompts. Nothing is done while the host says an
// approval card is open (Options.Busy).
//
// Windows only; elsewhere New returns ErrUnsupported.
package desktop

import (
	"errors"
	"regexp"
	"strings"

	"github.com/xattribution/gohelm/policy"
)

// Options configure the driver.
type Options struct {
	// Rules list the programs the agent may use (required).
	Rules *policy.DesktopRules
	// Confirm is the browser rules' Confirm level, applied here too.
	Confirm *policy.Rules
	// ExcludePIDs are the host's own processes besides this one (which is
	// always excluded).
	ExcludePIDs []int
	// MaxChars caps a snapshot (default 6000).
	MaxChars int
	// Busy, if set, says the host is showing an approval card: actions
	// wait until it's answered.
	Busy func() bool
	// AgentName names the agent on the user's cards. Default "the
	// assistant".
	AgentName string
}

// Window is a top-level window of an allowed program.
type Window struct {
	ID    string `json:"id"` // "w" + the window handle in hex
	Title string `json:"title"`
	App   string `json:"app"` // program file name
	PID   int    `json:"pid"`
}

// Outcome is what an action did.
type Outcome struct {
	Text    string
	Summary string
	Pending *policy.Ask
	Refused bool
}

// ErrUnsupported is returned off Windows.
var ErrUnsupported = errors.New("desktop: control of other programs needs Windows")

// blockedApps are never read or touched, whatever the rules say.
var blockedApps = map[string]bool{
	"consent.exe": true, "credentialuibroker.exe": true, "logonui.exe": true, "lockapp.exe": true,
	"securityhealthsystray.exe": true, "smartscreen.exe": true, "acsagent.exe": true, "acevents.exe": true,
	"userinit.exe": true, "winlogon.exe": true, "taskmgr.exe": true, "regedit.exe": true, "mmc.exe": true,
}

var blockedTitles = regexp.MustCompile(`(?i)(windows security|user account control|smart ?card|enter (your )?pin\b|\bpin required|activclient|credential manager|windows hello)`)

// blocked says a window may never be read or touched.
func blocked(app, title string) bool {
	return blockedApps[strings.ToLower(app)] || blockedTitles.MatchString(title)
}

// webRole maps a UI Automation control type to the role names the browser
// side uses, so one classifier serves both.
func webRole(ct string) string {
	switch ct {
	case "checkbox", "radio", "combobox", "slider", "tab", "treeitem":
		return ct
	case "edit":
		return "textbox"
	case "listitem", "dataitem":
		return "option"
	case "hyperlink":
		return "link"
	case "menuitem":
		return "menuitem"
	case "spinner":
		return "spinbutton"
	}
	return "button"
}
