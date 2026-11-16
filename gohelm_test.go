package gohelm

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xattribution/gohelm/internal/testsite"
	"github.com/xattribution/gohelm/launch"
	"github.com/xattribution/gohelm/policy"
)

func TestToolSets(t *testing.T) {
	s, err := New(Config{Profile: Pane, PaneDir: "/tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range s.Tools() {
		names = append(names, tl.Name)
		if tl.Description == "" || tl.Label == "" || tl.Run == nil || tl.Params == nil {
			t.Errorf("%s is incomplete", tl.Name)
		}
		for _, r := range tl.Required {
			if _, ok := tl.Params[r]; !ok {
				t.Errorf("%s requires %s, which isn't a parameter", tl.Name, r)
			}
		}
		if tl.ReadOnly && tl.Changes {
			t.Errorf("%s is both read-only and changing", tl.Name)
		}
	}
	want := "browser_go browser_snapshot browser_read browser_click browser_fill browser_press browser_dialog browser_hand_over"
	if strings.Join(names, " ") != want {
		t.Errorf("pane tools: %v", names)
	}
	full, _ := New(Config{Profile: Full, Vision: true})
	if n := len(full.Tools()); n != 14 {
		t.Errorf("full tools: %d", n)
	}
	for _, tl := range full.Tools() {
		if strings.HasPrefix(tl.Name, "browser_go") {
			if _, ok := tl.Params["where"]; !ok {
				t.Error("full browser_go lacks where")
			}
		}
	}
	if _, err := New(Config{Profile: Pane}); err == nil {
		t.Error("pane without a profile folder accepted")
	}
}

func TestOneOwnerAtATime(t *testing.T) {
	s, _ := New(Config{Profile: Pane, PaneDir: "/tmp/x"})
	if err := s.claim("chat-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.claim("chat-2"); err == nil {
		t.Error("second owner allowed")
	}
	s.Release("chat-1")
	if err := s.claim("chat-2"); err != nil {
		t.Error(err)
	}
}

// TestToolsEndToEnd drives the tools through the outside browser (the
// pane uses the same page code; it's tested on Windows).
func TestToolsEndToEnd(t *testing.T) {
	if _, err := launch.Find(); err != nil {
		t.Skip("no Edge or Chrome here")
	}
	site, err := testsite.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer site.Close()
	dir := t.TempDir()
	s, err := New(Config{Profile: Full, BrowserDir: filepath.Join(dir, "b"), BrowserHeadless: true, Vision: true,
		BrowserArgs: []string{"--host-resolver-rules=MAP *.test 127.0.0.1", "--no-proxy-server"},
		Rules:       policy.Rules{Allow: []string{"site.test"}, AllowHTTP: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tools := map[string]Tool{}
	for _, tl := range s.Tools() {
		tools[tl.Name] = tl
	}
	var mu sync.Mutex
	var asked []policy.Ask
	approved := map[string]bool{}
	env := policy.Env{Owner: "chat-1", Gate: policy.GateFunc(func(_ context.Context, a policy.Ask) (policy.Answer, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, a)
		if approved[a.Key] {
			return policy.Allow, nil
		}
		return policy.Pending, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	run := func(name string, a Args) Result {
		t.Helper()
		r, err := tools[name].Run(ctx, env, a)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return r
	}
	r := run("browser_go", Args{"url": site.URL("site.test", "/form"), "where": "browser"})
	if r.Refused || !strings.Contains(r.Text, `button "Continue"`) {
		t.Fatalf("open: %+v", r)
	}
	ref := func(text, want string) string {
		for _, l := range strings.Split(text, "\n") {
			if strings.Contains(l, want) {
				i := strings.Index(l, "[ref=")
				j := strings.Index(l[i:], "]")
				return l[i+5 : i+j]
			}
		}
		t.Fatalf("no %q in %s", want, text)
		return ""
	}
	name := ref(r.Text, `textbox "Full name"`)
	cont := ref(r.Text, `button "Continue"`)
	r = run("browser_fill", Args{"fields": []any{map[string]any{"ref": name, "value": "Pat Doe"}}})
	if r.Refused {
		t.Fatalf("fill: %s", r.Text)
	}
	r = run("browser_click", Args{"ref": cont})
	if r.Pending == nil {
		t.Fatalf("submit didn't wait: %s", r.Text)
	}
	approved[r.Pending.Key] = true
	r = run("browser_click", Args{"ref": cont})
	if r.Pending != nil || len(site.Posts()) != 1 {
		t.Fatalf("after OK: %+v %v", r, site.Posts())
	}
	r = run("browser_read", Args{})
	if !strings.Contains(r.Text, "Request received") {
		t.Errorf("read: %s", r.Text)
	}
	r = run("browser_screenshot", Args{})
	if len(r.Image) < 100 || r.Image[0] != 0xFF {
		t.Errorf("screenshot: %d bytes", len(r.Image))
	}
	r = run("browser_tabs", Args{"action": "list"})
	if !strings.Contains(r.Text, "tab 1") {
		t.Errorf("tabs: %s", r.Text)
	}
	r = run("browser_hand_over", Args{"reason": "Sign in with your card."})
	if r.Pending == nil || r.Pending.Kind != policy.KindHandOver {
		t.Errorf("hand over: %+v", r)
	}
	// Another chat can't take the browser.
	other := env
	other.Owner = "chat-2"
	if r, _ := tools["browser_snapshot"].Run(ctx, other, Args{}); !r.Refused {
		t.Errorf("second chat used the browser: %s", r.Text)
	}
	run("browser_go", Args{"action": "close", "where": "browser"})
	if r, _ := tools["browser_snapshot"].Run(ctx, other, Args{}); r.Refused && strings.Contains(r.Text, "another chat") {
		t.Errorf("closing didn't release: %s", r.Text)
	}

}
