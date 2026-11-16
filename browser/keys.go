package browser

import (
	"context"
	"fmt"

	"github.com/xattribution/gohelm/policy"
)

type keyDef struct {
	key, code string
	keyCode   int
	text      string
	mods      int      // 1 Alt, 2 Control, 4 Meta, 8 Shift
	commands  []string // editing commands Chrome runs for the shortcut
}

var keyDefs = map[string]keyDef{
	"Enter":      {"Enter", "Enter", 13, "\r", 0, nil},
	"Tab":        {"Tab", "Tab", 9, "", 0, nil},
	"Shift+Tab":  {"Tab", "Tab", 9, "", 8, nil},
	"Escape":     {"Escape", "Escape", 27, "", 0, nil},
	"Backspace":  {"Backspace", "Backspace", 8, "", 0, []string{"deleteBackward"}},
	"Delete":     {"Delete", "Delete", 46, "", 0, []string{"deleteForward"}},
	"Space":      {" ", "Space", 32, " ", 0, nil},
	"ArrowUp":    {"ArrowUp", "ArrowUp", 38, "", 0, nil},
	"ArrowDown":  {"ArrowDown", "ArrowDown", 40, "", 0, nil},
	"ArrowLeft":  {"ArrowLeft", "ArrowLeft", 37, "", 0, nil},
	"ArrowRight": {"ArrowRight", "ArrowRight", 39, "", 0, nil},
	"Home":       {"Home", "Home", 36, "", 0, nil},
	"End":        {"End", "End", 35, "", 0, nil},
	"PageUp":     {"PageUp", "PageUp", 33, "", 0, nil},
	"PageDown":   {"PageDown", "PageDown", 34, "", 0, nil},
	"Control+A":  {"a", "KeyA", 65, "", 2, []string{"selectAll"}},
}

// keyClass is a key's class: keys that only move are Read; the rest change
// the page (and what they send is checked like any action's).
func keyClass(name string) policy.Class {
	switch name {
	case "Tab", "Shift+Tab", "Escape", "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", "Home", "End", "PageUp", "PageDown":
		return policy.Read
	}
	return policy.Input
}

// normalizeKey accepts a few spellings the model may use.
func normalizeKey(k string) string {
	switch k {
	case "Return", "enter", "return":
		return "Enter"
	case "Esc", "esc", "escape":
		return "Escape"
	case " ", "space", "Spacebar":
		return "Space"
	case "Ctrl+A", "Control+a", "Ctrl+a", "ctrl+a":
		return "Control+A"
	case "tab":
		return "Tab"
	case "Up", "Down", "Left", "Right":
		return "Arrow" + k
	}
	return k
}

func (p *Page) sendKey(ctx context.Context, name string) error {
	d, ok := keyDefs[name]
	if !ok {
		return fmt.Errorf("the key %q isn't one gohelm presses", name)
	}
	down := map[string]any{"type": "rawKeyDown", "key": d.key, "code": d.code, "windowsVirtualKeyCode": d.keyCode, "nativeVirtualKeyCode": d.keyCode, "modifiers": d.mods}
	if d.text != "" {
		down["type"] = "keyDown"
		down["text"] = d.text
		down["unmodifiedText"] = d.text
	}
	if len(d.commands) > 0 {
		down["commands"] = d.commands
	}
	if err := p.input(ctx, "Input.dispatchKeyEvent", down); err != nil {
		return err
	}
	if p.Dialog() != nil {
		return nil
	}
	up := map[string]any{"type": "keyUp", "key": d.key, "code": d.code, "windowsVirtualKeyCode": d.keyCode, "nativeVirtualKeyCode": d.keyCode, "modifiers": d.mods}
	return p.input(ctx, "Input.dispatchKeyEvent", up)
}
