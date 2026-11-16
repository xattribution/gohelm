// Package launch starts Microsoft Edge or Google Chrome for gohelm's Full
// profile, with a profile folder of its own, talking DevTools over pipes so
// no port is opened. Browsers since Chrome 136 refuse DevTools on the
// user's own default profile, and gohelm never asks for it.
//
// A computer whose browser policy turns off remote debugging
// (RemoteDebuggingAllowed = false, the CIS benchmark's setting) refuses
// this too; use the pane (package pane) there.
package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xattribution/gohelm/cdp"
	"github.com/xattribution/gohelm/cdp/pipeconn"
)

// Options say how to start the browser.
type Options struct {
	// Exec is the browser's program. Empty finds Edge, then Chrome.
	Exec string
	// ProfileDir is the browser's own profile folder (required): sign-ins
	// made there stay there, apart from the user's everyday browser.
	ProfileDir string
	// Headless runs without a window (tests).
	Headless bool
	// Args are extra switches. Switches that open a debugging port or
	// point at another profile are refused.
	Args []string
	// Width and Height of the window, default 1280×900.
	Width, Height int
}

// Browser is a running browser and its DevTools connection.
type Browser struct {
	Conn *cdp.Conn
	cmd  *exec.Cmd
	once sync.Once
	done chan struct{}
}

// ErrNoBrowser means neither Edge nor Chrome was found.
var ErrNoBrowser = errors.New("launch: Microsoft Edge or Google Chrome isn't installed")

// Find returns the first Edge or Chrome program on this computer.
func Find() (string, error) {
	for _, p := range candidates() {
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
			continue
		}
		if lp, err := exec.LookPath(p); err == nil {
			return lp, nil
		}
	}
	return "", ErrNoBrowser
}

func candidates() []string {
	switch runtime.GOOS {
	case "windows":
		pf, pf86, local := os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")
		var out []string
		for _, base := range []string{pf86, pf} {
			if base != "" {
				out = append(out, filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`))
			}
		}
		for _, base := range []string{pf, pf86, local} {
			if base != "" {
				out = append(out, filepath.Join(base, `Google\Chrome\Application\chrome.exe`))
			}
		}
		return out
	case "darwin":
		return []string{
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		return []string{"microsoft-edge", "microsoft-edge-stable", "google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	}
}

// refusedArg is a switch that would open DevTools to other programs or
// use another profile.
func refusedArg(a string) bool {
	a = strings.ToLower(strings.TrimLeft(a, "-"))
	for _, p := range []string{"remote-debugging-port", "remote-debugging-address", "remote-allow-origins", "user-data-dir", "remote-debugging-pipe", "remote-debugging-io-pipes", "profile-directory"} {
		if a == p || strings.HasPrefix(a, p+"=") {
			return true
		}
	}
	return false
}

// Start starts the browser and connects to it.
func Start(ctx context.Context, o Options) (*Browser, error) {
	if o.ProfileDir == "" {
		return nil, errors.New("launch: a profile folder is required")
	}
	if !filepath.IsAbs(o.ProfileDir) {
		return nil, errors.New("launch: the profile folder must be a full path")
	}
	exe := o.Exec
	if exe == "" {
		var err error
		if exe, err = Find(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(o.ProfileDir, 0o700); err != nil {
		return nil, err
	}
	w, h := o.Width, o.Height
	if w <= 0 || h <= 0 {
		w, h = 1280, 900
	}
	args := []string{
		"--user-data-dir=" + o.ProfileDir,
		"--remote-debugging-pipe",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
		"--disable-extensions",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-features=Translate,MediaRouter,OptimizationHints",
		"--password-store=basic",
		fmt.Sprintf("--window-size=%d,%d", w, h),
	}
	if o.Headless {
		args = append(args, "--headless=new", "--hide-scrollbars", "--mute-audio")
		if runtime.GOOS == "linux" && os.Geteuid() == 0 {
			args = append(args, "--no-sandbox") // a container's root can't use the sandbox
		}
	}
	for _, a := range o.Args {
		if refusedArg(a) {
			return nil, fmt.Errorf("launch: %s isn't allowed", a)
		}
		args = append(args, a)
	}
	args = append(args, "about:blank")
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	mc, err := startWithPipes(cmd)
	if err != nil {
		return nil, fmt.Errorf("launch: starting %s: %w", filepath.Base(exe), err)
	}
	b := &Browser{Conn: cdp.NewConn(mc), cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); close(b.done) }()
	// The first answer proves the pipe works; a browser whose policy turns
	// DevTools off exits or never answers.
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := b.Conn.Call(vctx, "Browser.getVersion", nil); err != nil {
		b.Close()
		return nil, fmt.Errorf("launch: the browser didn't answer on its DevTools pipe (its policy may turn remote debugging off): %w", err)
	}
	return b, nil
}

// Version is the browser's product string.
func (b *Browser) Version(ctx context.Context) (string, error) {
	var v struct {
		Product string `json:"product"`
	}
	err := cdp.Do(ctx, b.Conn, "Browser.getVersion", nil, &v)
	return v.Product, err
}

// Target is one tab or window the browser has.
type Target struct {
	ID       string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Attached bool   `json:"attached"`
}

// Pages lists the browser's tabs.
func (b *Browser) Pages(ctx context.Context) ([]Target, error) {
	var r struct {
		TargetInfos []Target `json:"targetInfos"`
	}
	if err := cdp.Do(ctx, b.Conn, "Target.getTargets", nil, &r); err != nil {
		return nil, err
	}
	var out []Target
	for _, t := range r.TargetInfos {
		if t.Type == "page" {
			out = append(out, t)
		}
	}
	return out, nil
}

// NewPage opens a tab at url (about:blank when empty).
func (b *Browser) NewPage(ctx context.Context, url string) (string, error) {
	if url == "" {
		url = "about:blank"
	}
	var r struct {
		TargetID string `json:"targetId"`
	}
	err := cdp.Do(ctx, b.Conn, "Target.createTarget", map[string]any{"url": url}, &r)
	return r.TargetID, err
}

// Attach returns a Transport for one tab.
func (b *Browser) Attach(ctx context.Context, targetID string) (cdp.Transport, error) {
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := cdp.Do(ctx, b.Conn, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &r); err != nil {
		return nil, err
	}
	return b.Conn.Session(r.SessionID), nil
}

// ClosePage closes one tab.
func (b *Browser) ClosePage(ctx context.Context, targetID string) error {
	_, err := b.Conn.Call(ctx, "Target.closeTarget", map[string]any{"targetId": targetID})
	return err
}

// Exited is closed when the browser's process ends.
func (b *Browser) Exited() <-chan struct{} { return b.done }

// Close asks the browser to quit, then ends it if it hasn't within a few
// seconds.
func (b *Browser) Close() error {
	b.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		b.Conn.Call(ctx, "Browser.close", nil)
		cancel()
		select {
		case <-b.done:
		case <-time.After(5 * time.Second):
			if b.cmd.Process != nil {
				b.cmd.Process.Kill()
			}
			<-b.done
		}
		b.Conn.Close()
	})
	return nil
}

// json is used by the platform files.
var _ = json.Marshal

var _ cdp.MessageConn = (*pipeconn.Conn)(nil)
