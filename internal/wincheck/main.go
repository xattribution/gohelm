//go:build windows

// wincheck runs gohelm's Windows-only parts against the real thing: the
// WebView2 pane on public test sites, and the desktop driver on a small
// test window. Run it on a Windows computer:
//
//	go build -o testapp.exe ./internal/wincheck/testapp
//	go run ./internal/wincheck -testapp testapp.exe
//
// It opens windows on the screen for a few seconds. It never touches the
// user's own programs: the desktop checks use only the test window.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xattribution/gohelm"
	"github.com/xattribution/gohelm/browser"
	"github.com/xattribution/gohelm/desktop"
	"github.com/xattribution/gohelm/pane"
	"github.com/xattribution/gohelm/policy"
)

var failed int

func check(ok bool, what string, detail ...any) {
	if ok {
		fmt.Println("PASS", what)
		return
	}
	failed++
	fmt.Println("FAIL", what, fmt.Sprint(detail...))
}

// gate answers Pending until a key is approved, like a host's card.
type gate struct {
	mu       sync.Mutex
	approved map[string]bool
	asked    []policy.Ask
}

func (g *gate) Decide(_ context.Context, a policy.Ask) (policy.Answer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, a)
	if g.approved[a.Key] {
		delete(g.approved, a.Key)
		return policy.Allow, nil
	}
	return policy.Pending, nil
}

func refOf(text, want string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, want) {
			if i := strings.Index(l, "[ref="); i >= 0 {
				j := strings.Index(l[i:], "]")
				return l[i+5 : i+j]
			}
		}
	}
	return ""
}

func main() {
	testapp := flag.String("testapp", "", "path to testapp.exe")
	skipPane := flag.Bool("skip-pane", false, "skip the pane checks")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dir, _ := os.MkdirTemp("", "gohelm-wincheck-")
	defer os.RemoveAll(dir)
	if !*skipPane {
		paneChecks(ctx, dir)
	}
	if *testapp != "" {
		desktopChecks(ctx, *testapp)
	}
	if failed > 0 {
		fmt.Println(failed, "FAILED")
		os.Exit(1)
	}
	fmt.Println("ALL PASSED")
}

func paneChecks(ctx context.Context, dir string) {
	check(pane.Available(), "WebView2 runtime found")
	rules := policy.Rules{Allow: []string{"example.com", "iana.org", "httpbin.org", "client.badssl.com"}}
	var certAsked atomic.Int32
	var closed atomic.Bool
	w, err := pane.Open(ctx, pane.Options{Title: "gohelm check", DataDir: filepath.Join(dir, "pane"),
		AllowNavigation: func(u string, top, user bool) bool { return rules.Check(u).Verdict != policy.Refused },
		Certificate:     func(host string, port int) bool { certAsked.Add(1); return false },
		OnClosed:        func() { closed.Store(true) }})
	check(err == nil, "pane opens", err)
	if err != nil {
		return
	}
	defer w.Close()
	w.Show()
	w.SetStatus("gohelm check is working")
	g := &gate{approved: map[string]bool{}}
	env := policy.Env{Gate: g}
	pg, err := browser.Attach(ctx, w.Transport(), browser.Options{Rules: &rules})
	check(err == nil, "page attaches over WebView2's DevTools API", err)
	if err != nil {
		return
	}
	o, err := pg.Navigate(ctx, env, "https://example.com/")
	check(err == nil && !o.Refused && strings.Contains(o.Text, "Example Domain"), "opens example.com", err, o.Text)
	fmt.Println(o.Text)
	link := refOf(o.Text, "link ")
	check(link != "", "snapshot has a link ref")
	o, _ = pg.Click(ctx, env, link, false)
	check(!o.Refused && strings.Contains(pg.URL(), "iana.org"), "clicking a link follows it", o.Text)

	o, _ = pg.Navigate(ctx, env, "https://www.google.com/")
	check(o.Refused, "a site off the list is refused", o.Text)
	o, _ = pg.Navigate(ctx, env, "http://127.0.0.1:47731/")
	check(o.Refused, "the host's own address is refused", o.Text)

	o, err = pg.Navigate(ctx, env, "https://httpbin.org/forms/post")
	check(err == nil && !o.Refused, "opens the httpbin form", err, o.Text)
	s, _ := pg.Snapshot(ctx, browser.SnapOptions{})
	fmt.Println(s.Text)
	name := refOf(s.Text, `textbox "Customer name`)
	submit := refOf(s.Text, `button "Submit order"`)
	check(name != "" && submit != "", "form fields have refs")
	o, _ = pg.Fill(ctx, env, []browser.FieldValue{{Ref: name, Value: "gohelm check"}})
	check(!o.Refused, "fills a field", o.Text)
	o, _ = pg.Click(ctx, env, submit, false)
	check(o.Pending != nil && o.Pending.Kind == policy.KindCommit, "submit waits for the user's OK", o.Text)
	if o.Pending != nil {
		g.approved[o.Pending.Key] = true
		o, _ = pg.Click(ctx, env, submit, false)
		check(!o.Refused && o.Pending == nil && strings.Contains(o.Text, "gohelm check"), "after the OK, the form posts", o.Text)
	}

	pg.Navigate(ctx, env, "https://client.badssl.com/")
	time.Sleep(time.Second)
	check(certAsked.Load() > 0, "a certificate request reaches the host's check")

	// The session wiring, with gohelm opening the pane itself.
	sess, err := gohelm.New(gohelm.Config{Profile: gohelm.Pane, PaneDir: filepath.Join(dir, "pane2"), PaneTitle: "gohelm session check",
		Rules: policy.Rules{Allow: []string{"example.com"}}})
	check(err == nil, "session", err)
	tools := map[string]gohelm.Tool{}
	for _, t := range sess.Tools() {
		tools[t.Name] = t
	}
	r, err := tools["browser_go"].Run(ctx, env, gohelm.Args{"url": "https://example.com"})
	check(err == nil && strings.Contains(r.Text, "Example Domain"), "browser_go through the session opens the pane", err, r.Text)
	r, _ = tools["browser_read"].Run(ctx, env, gohelm.Args{})
	check(strings.Contains(r.Text, "illustrative examples") || strings.Contains(r.Text, "documentation"), "browser_read", r.Text)
	sess.Close()
	w.Close()
	time.Sleep(300 * time.Millisecond)
	check(closed.Load(), "the pane reports closing")
}

func desktopChecks(ctx context.Context, testapp string) {
	cmd := exec.Command(testapp)
	if err := cmd.Start(); err != nil {
		check(false, "start test app", err)
		return
	}
	defer cmd.Process.Kill()
	time.Sleep(1500 * time.Millisecond)
	exe := filepath.Base(testapp)
	g := &gate{approved: map[string]bool{}}
	env := policy.Env{Gate: g}
	d, err := desktop.New(desktop.Options{Rules: &policy.DesktopRules{Apps: []string{exe}}, Confirm: &policy.Rules{}})
	check(err == nil, "desktop driver starts", err)
	if err != nil {
		return
	}
	defer d.Close()
	ws, others, err := d.Windows(ctx)
	check(err == nil && len(ws) >= 1, "lists the test app's window", err, ws)
	fmt.Println("windows:", ws, "others:", others)
	var id string
	for _, w := range ws {
		if strings.Contains(w.Title, "gohelm test app") {
			id = w.ID
		}
	}
	check(id != "", "finds the test window")
	if id == "" {
		return
	}
	o, _ := d.Snapshot(ctx, env, id, "")
	check(o.Pending != nil, "first use of a program asks the user", o.Text)
	if o.Pending != nil {
		g.approved[o.Pending.Key] = true
		o, _ = d.Snapshot(ctx, env, id, "")
	}
	fmt.Println(o.Text)
	edit := refOf(o.Text, "- edit")
	box := refOf(o.Text, `checkbox "Remember me"`)
	send := refOf(o.Text, `button "Send report"`)
	check(edit != "" && box != "" && send != "", "snapshot has the controls with refs", o.Text)
	o, _ = d.Act(ctx, env, edit, "set_value", "hello from gohelm")
	check(!o.Refused && strings.Contains(o.Text, "hello from gohelm"), "set_value types through the Value pattern", o.Text)
	o, _ = d.Act(ctx, env, refOf(o.Text, `checkbox "Remember me"`), "toggle", "")
	check(!o.Refused && strings.Contains(o.Text, `checkbox "Remember me"`) && strings.Contains(o.Text, "[checked]"), "toggle checks the box", o.Text)
	send = refOf(o.Text, `button "Send report"`)
	o, _ = d.Act(ctx, env, send, "click", "")
	check(o.Pending != nil, "clicking Send waits for the user's OK", o.Text)
	if o.Pending != nil {
		g.approved[o.Pending.Key] = true
		o, _ = d.Act(ctx, env, send, "click", "")
		check(!o.Refused && strings.Contains(o.Text, "sent"), "after the OK, Send is invoked", o.Text)
	}
	img, err := d.Screenshot(ctx, env, id)
	check(err == nil && len(img) > 500, "screenshot of the window", err)
	// Its own windows are never listed.
	sess, _ := gohelm.New(gohelm.Config{Profile: gohelm.Full, Rules: policy.Rules{Desktop: policy.DesktopRules{Apps: []string{exe, filepath.Base(os.Args[0])}}}})
	tools := map[string]gohelm.Tool{}
	for _, t := range sess.Tools() {
		tools[t.Name] = t
	}
	r, _ := tools["desktop_windows"].Run(ctx, env, gohelm.Args{})
	check(strings.Contains(r.Text, "gohelm test app") && !strings.Contains(r.Text, "gohelm check"), "desktop_windows lists the app and never the host's own windows", r.Text)
	sess.Close()
}
