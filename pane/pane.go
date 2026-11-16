// Package pane is a browser window the host program owns: Microsoft Edge
// WebView2, driven only through WebView2's own DevTools API
// (ICoreWebView2::CallDevToolsProtocolMethod). No debugging port is opened,
// so nothing else on the computer can attach to it, and an Edge policy that
// turns off remote debugging doesn't stop it.
//
// The window has a profile folder of its own, apart from the host's own
// WebView2 window and the user's Edge, with Windows-account sign-in off,
// password saving and autofill off, downloads and permission prompts
// refused, and pop-ups opened in the same window after the host checks
// them. When a site asks for the user's certificate (CAC), the host
// decides whether the site may ask; if yes, WebView2 shows its own picker
// and Windows its own PIN prompt. gohelm never chooses a certificate and
// never sees a PIN.
//
// Windows only; elsewhere Open returns ErrUnsupported.
package pane

import "errors"

// Options configure the window.
type Options struct {
	// Title is the window's title before its status ("Quartermaster
	// browser").
	Title string
	// DataDir is the window's own WebView2 profile folder (required). Never
	// the host's own WebView2 folder.
	DataDir string
	// Owner is the host's window, which this one stays in front of. Zero
	// for none.
	Owner uintptr
	// AllowNavigation is asked, on the window's thread, before every
	// navigation of the page or a frame: return false to cancel it. It
	// must answer quickly (check rules; never ask the user here).
	AllowNavigation func(url string, topLevel, userInitiated bool) bool
	// Certificate is asked when a site asks for the user's certificate.
	// Return true to let WebView2 show its certificate picker. It runs on
	// its own goroutine and may wait.
	Certificate func(host string, port int) bool
	// OnClosed is called once when the window closes (by the user or by
	// Close).
	OnClosed func()
}

// ErrUnsupported is returned where WebView2 isn't available.
var ErrUnsupported = errors.New("pane: the browser window needs Windows with the WebView2 runtime")
