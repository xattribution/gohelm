package policy

import (
	"regexp"
	"strings"
)

// Class is how much an action can change.
type Class int

const (
	// Read only looks: snapshots, reading, scrolling, waiting.
	Read Class = iota
	// Input changes the page but sends nothing: typing, choosing, checking.
	Input
	// Commit sends or changes something outside the page.
	Commit
	// Secret is typing a password, PIN, one-time code, card number or ID
	// number. The agent never does it; the user does (hand-over).
	Secret
)

func (c Class) String() string {
	return [...]string{"read", "input", "commit", "secret"}[c]
}

// commitWords are names of buttons and links that send or change
// something. The list is a first line only: what the page actually sends
// after an action is gated too (package browser).
var commitWords = regexp.MustCompile(`(?i)\b(submit|send|post|publish|pay|purchase|buy|order|checkout|check out|place order|delete|remove|erase|discard|destroy|sign|e-?sign|certify|approve|reject|deny|confirm|transfer|wire|withdraw|deposit|apply|book|reserve|enroll|register|unsubscribe|cancel (my )?(account|subscription|order|reservation)|accept|agree|authorize|grant|share|invite|forward|reply|upload|save|update|finish|complete|request)\b`)

// ClickClass is the class of clicking an element, from what the page says
// about it. submit is true for a submit button in a form.
func ClickClass(role, name string, submit bool) Class {
	if submit {
		return Commit
	}
	switch strings.ToLower(role) {
	case "checkbox", "radio", "switch", "option", "menuitemcheckbox", "menuitemradio", "tab", "combobox", "listbox", "treeitem", "textbox", "searchbox", "spinbutton", "slider":
		return Input
	}
	if commitWords.MatchString(name) {
		return Commit
	}
	return Read
}

// secretWords mark a field the agent never types into, by its label, name,
// id or placeholder.
var secretWords = regexp.MustCompile(`(?i)(pass(word|code|phrase)?\b|\bpwd\b|\bpin\b|pin ?code|\bpin[ _-]?(number|no)\b|one[- ]?time|\botp\b|verification code|security code|\bcvv\b|\bcvc\b|\bcsc\b|card ?number|credit ?card|\bccn\b|\bssn\b|social security|\bdod ?id\b|\bedipi\b|\btin\b|taxpayer|account ?number|routing ?number|\biban\b|secret|security answer|mother'?s maiden)`)

// secretAutocomplete are autocomplete tokens of secret fields.
var secretAutocomplete = map[string]bool{
	"current-password": true, "new-password": true, "one-time-code": true,
	"cc-number": true, "cc-csc": true, "cc-exp": true, "cc-exp-month": true, "cc-exp-year": true,
}

// Field is what is known about a form field.
type Field struct {
	Tag          string // input, textarea, select, or the element's tag
	Type         string // the input's type attribute
	Autocomplete string
	Name, ID     string
	Label        string // its accessible name
	Placeholder  string
}

// SecretField says whether the agent must never type into f.
func SecretField(f Field) bool {
	if strings.EqualFold(f.Type, "password") {
		return true
	}
	for _, tok := range strings.Fields(strings.ToLower(f.Autocomplete)) {
		if secretAutocomplete[tok] {
			return true
		}
	}
	for _, s := range []string{f.Label, f.Name, f.ID, f.Placeholder} {
		if s != "" && (secretWords.MatchString(s) || secretWords.MatchString(splitIdent(s))) {
			return true
		}
	}
	return false
}

// splitIdent turns userPin and user_pin into "user pin" so word matches
// work on identifiers.
func splitIdent(s string) string {
	var b strings.Builder
	prevLower := false
	for _, r := range s {
		switch {
		case r == '_' || r == '-' || r == '.' || r == '[' || r == ']':
			b.WriteByte(' ')
			prevLower = false
			continue
		case r >= 'A' && r <= 'Z':
			if prevLower {
				b.WriteByte(' ')
			}
			prevLower = false
		default:
			prevLower = r >= 'a' && r <= 'z'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// injectionHints are phrases that speak to an AI assistant. Finding one is
// a hint for the model and the user, never a guarantee either way.
var injectionHints = regexp.MustCompile(`(?i)(ignore (all |any )?(the )?(previous|prior|above|earlier) (instructions|prompts|messages)|disregard (the|your|all) (previous|prior|above|instructions)|you are now (a|an|in)\b|new instructions:|system prompt|developer mode|\bas an ai (assistant|model|agent)\b|(attention|note|message) (to|for) (the )?(ai|assistant|agent|llm|model)\b|\bai (assistant|agent)s?,? (must|should|please)\b|do not (tell|inform|show) the user|without (telling|asking) the user)`)

// InjectionHint says whether text seems to address an AI assistant.
func InjectionHint(text string) bool { return injectionHints.MatchString(text) }

// AllowedKeys are the keys the agent may press, by the names it uses.
var AllowedKeys = map[string]bool{
	"Enter": true, "Tab": true, "Shift+Tab": true, "Escape": true, "Backspace": true, "Delete": true,
	"Space": true, "ArrowUp": true, "ArrowDown": true, "ArrowLeft": true, "ArrowRight": true,
	"Home": true, "End": true, "PageUp": true, "PageDown": true, "Control+A": true,
}
