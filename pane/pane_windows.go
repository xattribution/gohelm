//go:build windows

package pane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/xattribution/gohelm/cdp"
)

// The steps of Microsoft's WebView2 loader (find the runtime, create its
// environment) are done here in Go, as Quartermaster's own window does
// them, so nothing ships with the program.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")

	pRegisterClassEx = user32.NewProc("RegisterClassExW")
	pCreateWindowEx  = user32.NewProc("CreateWindowExW")
	pDefWindowProc   = user32.NewProc("DefWindowProcW")
	pDestroyWindow   = user32.NewProc("DestroyWindow")
	pShowWindow      = user32.NewProc("ShowWindow")
	pIsIconic        = user32.NewProc("IsIconic")
	pGetClientRect   = user32.NewProc("GetClientRect")
	pPostMessage     = user32.NewProc("PostMessageW")
	pGetMessage      = user32.NewProc("GetMessageW")
	pTranslate       = user32.NewProc("TranslateMessage")
	pDispatch        = user32.NewProc("DispatchMessageW")
	pPostQuit        = user32.NewProc("PostQuitMessage")
	pSetWindowText   = user32.NewProc("SetWindowTextW")
	pLoadCursor      = user32.NewProc("LoadCursorW")
	pGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
	pCoInitEx        = ole32.NewProc("CoInitializeEx")
	pCoTaskMemAlloc  = ole32.NewProc("CoTaskMemAlloc")
	pCoTaskMemFree   = ole32.NewProc("CoTaskMemFree")
)

const minVersion = "86.0.616.0"

// runtimeDLL finds the installed WebView2 runtime's client DLL: the
// stable channel's ClientState key, machine first, then the user's.
func runtimeDLL() (string, error) {
	const key = `Software\Microsoft\EdgeUpdate\ClientState\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		k, err := registry.OpenKey(root, key, registry.READ|registry.WOW64_32KEY)
		if err != nil {
			continue
		}
		dir, _, err := k.GetStringValue("EBWebView")
		k.Close()
		if err != nil || dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		if compareVersions(filepath.Base(dir), minVersion) < 0 {
			continue
		}
		arch := "x64"
		if runtime.GOARCH == "arm64" {
			arch = "arm64"
		}
		dll := filepath.Join(dir, "EBWebView", arch, "EmbeddedBrowserWebView.dll")
		if _, err := os.Stat(dll); err == nil {
			return dll, nil
		}
	}
	return "", errors.New("pane: the WebView2 runtime isn't installed")
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 4; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Available says whether the window can be opened here.
func Available() bool {
	_, err := runtimeDLL()
	return err == nil
}

// ---- COM ----

func comCall(obj unsafe.Pointer, slot int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func comRelease(obj unsafe.Pointer) {
	if obj != nil {
		comCall(obj, 2)
	}
}

func coString(p *uint16) string {
	if p == nil {
		return ""
	}
	s := windows.UTF16PtrToString(p)
	pCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return s
}

func coAlloc(s string) *uint16 {
	w, _ := windows.UTF16FromString(s)
	r, _, _ := pCoTaskMemAlloc.Call(uintptr(len(w) * 2))
	if r == 0 {
		return nil
	}
	p := *(**uint16)(unsafe.Pointer(&r))
	copy(unsafe.Slice(p, len(w)), w)
	return p
}

func guid(s string) windows.GUID { g, _ := windows.GUIDFromString(s); return g }

var (
	iidUnknown       = guid("{00000000-0000-0000-C000-000000000046}")
	iidEnvCompleted  = guid("{4e8a3389-c9d8-4bd2-b6b5-124fee6cc14d}")
	iidCtrlCompleted = guid("{6c4819f3-c9b7-4260-8127-c9f5bde7f68c}")
	iidEnvOptions    = guid("{2fde08a8-1e9a-4766-8c05-95a9ceb9d1c5}")
	iidNavStarting   = guid("{9adbe429-f36d-432b-9ddc-f8881fbd76e3}")
	iidNewWindow     = guid("{d4c185fe-c81c-4989-97af-2d3fa7ab5651}")
	iidPermission    = guid("{15e1c6a3-c72a-4df3-91d7-d097fbec6bfd}")
	iidCDPCompleted  = guid("{5c4889f0-5ef6-4c5a-952c-d8f1b92d0574}")
	iidCDPEvent      = guid("{e2fda4be-5456-406c-a261-3d452138362c}")
	iidDownload      = guid("{efedc989-c396-41ca-83f7-07f845a55724}")
	iidCertRequested = guid("{d7175ba2-bcc3-11eb-8529-0242ac130003}")
	iidTitleChanged  = guid("{f5f2b923-953e-4042-9f95-f3a118e1afd4}")
	iidWebView4      = guid("{20d02d59-6df2-42dc-bd06-f98a694b1302}")
	iidWebView5      = guid("{bedb11b8-d63c-11eb-b8bc-0242ac130003}")
	iidSettings3     = guid("{fdb5ab74-af33-4854-84f0-0a631deb5eba}")
	iidSettings4     = guid("{cb56846c-4168-4d53-b04f-03b6d6796ff2}")
)

// Slots (WebView2.idl order, after IUnknown's three).
const (
	envCreateController = 3

	ctrlPutIsVisible   = 4
	ctrlPutBounds      = 6
	ctrlMoveFocus      = 12
	ctrlNotifyMoved    = 23
	ctrlClose          = 24
	ctrlGetCoreWebView = 25

	wvGetSettings          = 3
	wvNavigate             = 5
	wvAddNavStarting       = 7
	wvAddFrameNavStarting  = 17
	wvAddPermission        = 23
	wvCallDevTools         = 36
	wvGetDevToolsReceiver  = 42
	wvAddNewWindow         = 44
	wvAddTitleChanged      = 46
	wvGetDocumentTitle     = 48
	wv4AddDownloadStarting = 75
	wv5AddCertRequested    = 77

	setPutWebMessage    = 6
	setPutStatusBar     = 10
	setPutDevTools      = 12
	setPutHostObjects   = 16
	set3PutAccelerators = 24
	set4PutPasswordSave = 26
	set4PutAutofill     = 28

	navGetURI       = 3
	navGetUserInit  = 4
	navPutCancel    = 8
	nwGetURI        = 3
	nwPutHandled    = 6
	permPutState    = 7
	dlPutCancel     = 5
	certGetHost     = 3
	certGetPort     = 4
	certPutCancel   = 11
	certGetDeferral = 14
	defComplete     = 3
	recvAdd         = 3
	evArgsGetJSON   = 3
)

// comObj is a COM object made in Go: its function table first, then what
// the functions need. It stays in keep while anything holds a reference.
type comObj struct {
	vtbl   *[11]uintptr
	refs   int32
	iid    windows.GUID
	invoke func(a uintptr, b unsafe.Pointer) uintptr
}

var (
	keepMu    sync.Mutex
	keep      = map[*comObj]bool{}
	vtHandler [11]uintptr
	vtOptions [11]uintptr
	vtOnce    sync.Once
	wndProcCB uintptr
)

func comQI(this *comObj, iid *windows.GUID, out *unsafe.Pointer) uintptr {
	if *iid == iidUnknown || *iid == this.iid {
		atomic.AddInt32(&this.refs, 1)
		*out = unsafe.Pointer(this)
		return 0
	}
	*out = nil
	return 0x80004002 // E_NOINTERFACE
}

func comAddRef(this *comObj) uintptr { return uintptr(atomic.AddInt32(&this.refs, 1)) }

func comRel(this *comObj) uintptr {
	n := atomic.AddInt32(&this.refs, -1)
	if n <= 0 {
		keepMu.Lock()
		delete(keep, this)
		keepMu.Unlock()
		return 0
	}
	return uintptr(n)
}

func comInvoke(this *comObj, a uintptr, b unsafe.Pointer) uintptr {
	if this.invoke != nil {
		return this.invoke(a, b)
	}
	return 0
}

func optArgs(_ *comObj, v **uint16) uintptr    { *v = coAlloc(""); return 0 }
func optLang(_ *comObj, v **uint16) uintptr    { *v = coAlloc(""); return 0 }
func optVersion(_ *comObj, v **uint16) uintptr { *v = coAlloc(minVersion); return 0 }
func optSSO(_ *comObj, v *int32) uintptr       { *v = 0; return 0 } // never sign in with the Windows account
func optPut(_ *comObj, _ uintptr) uintptr      { return 0 }

func initCallbacks() {
	vtOnce.Do(func() {
		qi, ar, rl := windows.NewCallback(comQI), windows.NewCallback(comAddRef), windows.NewCallback(comRel)
		vtHandler = [11]uintptr{qi, ar, rl, windows.NewCallback(comInvoke)}
		put := windows.NewCallback(optPut)
		vtOptions = [11]uintptr{qi, ar, rl, windows.NewCallback(optArgs), put, windows.NewCallback(optLang), put,
			windows.NewCallback(optVersion), put, windows.NewCallback(optSSO), put}
		wndProcCB = windows.NewCallback(wndProc)
	})
}

func newObj(vt *[11]uintptr, iid windows.GUID, invoke func(a uintptr, b unsafe.Pointer) uintptr) *comObj {
	o := &comObj{vtbl: vt, refs: 1, iid: iid, invoke: invoke}
	keepMu.Lock()
	keep[o] = true
	keepMu.Unlock()
	return o
}

func (o *comObj) ptr() uintptr { return uintptr(unsafe.Pointer(o)) }

// ---- the window ----

// Window is the browser window.
type Window struct {
	o    Options
	hwnd uintptr
	ctrl unsafe.Pointer
	wv   unsafe.Pointer

	qmu   sync.Mutex
	queue []func()

	smu       sync.Mutex
	subs      map[string]map[int]func(json.RawMessage)
	subID     int
	receivers map[string]bool
	evq       *cdp.Queue

	ready   chan error
	done    chan struct{}
	closing atomic.Bool
	once    sync.Once
	status  string
	title   string
}

const wmRun = 0x8000 + 7 // WM_APP + 7

var (
	winMu sync.Mutex
	panes = map[uintptr]*Window{}
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type rect struct{ left, top, right, bottom int32 }

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      [2]int32
	private uint32
}

var classOnce sync.Once

const className = "gohelmPane"

// Open makes the window, hidden until Show, and waits until its page is
// ready for DevTools.
func Open(ctx context.Context, o Options) (*Window, error) {
	if o.DataDir == "" || !filepath.IsAbs(o.DataDir) {
		return nil, errors.New("pane: a full path for the window's own profile folder is required")
	}
	dll, err := runtimeDLL()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return nil, err
	}
	if o.Title == "" {
		o.Title = "Browser"
	}
	initCallbacks()
	w := &Window{o: o, subs: map[string]map[int]func(json.RawMessage){}, receivers: map[string]bool{},
		evq: cdp.NewQueue(), ready: make(chan error, 1), done: make(chan struct{}), title: o.Title}
	go w.thread(dll)
	select {
	case err := <-w.ready:
		if err != nil {
			return nil, err
		}
		return w, nil
	case <-ctx.Done():
		w.Close()
		return nil, ctx.Err()
	}
}

func (w *Window) thread(dll string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer w.finish()
	pCoInitEx.Call(0, 2) // COINIT_APARTMENTTHREADED
	hinst, _, _ := pGetModuleHandle.Call(0)
	classOnce.Do(func() {
		cursor, _, _ := pLoadCursor.Call(0, 32512)
		cls, _ := windows.UTF16PtrFromString(className)
		wc := wndClassEx{style: 0x0003, lpfnWndProc: wndProcCB, hInstance: hinst, hCursor: cursor, hbrBackground: 6, lpszClassName: cls}
		wc.cbSize = uint32(unsafe.Sizeof(wc))
		pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	})
	cls, _ := windows.UTF16PtrFromString(className)
	title, _ := windows.UTF16PtrFromString(w.o.Title)
	const style = 0x00CF0000 // WS_OVERLAPPEDWINDOW
	const def = 0x80000000   // CW_USEDEFAULT
	hwnd, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), style, def, def, def, def, w.o.Owner, 0, hinst, 0)
	if hwnd == 0 {
		w.ready <- errors.New("pane: the window couldn't be made")
		return
	}
	w.hwnd = hwnd
	winMu.Lock()
	panes[hwnd] = w
	winMu.Unlock()

	create, err := windows.LoadDLL(dll)
	if err != nil {
		w.ready <- err
		pDestroyWindow.Call(hwnd)
		return
	}
	proc, err := create.FindProc("CreateWebViewEnvironmentWithOptionsInternal")
	if err != nil {
		w.ready <- err
		pDestroyWindow.Call(hwnd)
		return
	}
	dataDir, _ := windows.UTF16PtrFromString(w.o.DataDir)
	opts := newObj(&vtOptions, iidEnvOptions, nil)
	envDone := newObj(&vtHandler, iidEnvCompleted, func(code uintptr, envp unsafe.Pointer) uintptr {
		if uint32(code) != 0 || envp == nil {
			w.fail(fmt.Errorf("pane: WebView2 didn't start (0x%x)", uint32(code)))
			return 0
		}
		comCall(envp, 1) // kept for the window's life
		ctrlDone := newObj(&vtHandler, iidCtrlCompleted, func(code uintptr, ctrl unsafe.Pointer) uintptr {
			if uint32(code) != 0 || ctrl == nil {
				w.fail(fmt.Errorf("pane: WebView2's view didn't start (0x%x)", uint32(code)))
				return 0
			}
			comCall(ctrl, 1)
			w.setup(ctrl)
			return 0
		})
		if hr := comCall(envp, envCreateController, w.hwnd, ctrlDone.ptr()); hr != 0 {
			w.fail(fmt.Errorf("pane: WebView2's view couldn't be made (0x%x)", uint32(hr)))
		}
		comRel(ctrlDone)
		return 0
	})
	const runtimeInstalled = 0
	if hr, _, _ := proc.Call(1, runtimeInstalled, uintptr(unsafe.Pointer(dataDir)), opts.ptr(), envDone.ptr()); hr != 0 {
		w.ready <- fmt.Errorf("pane: WebView2 didn't start (0x%x)", uint32(hr))
		pDestroyWindow.Call(hwnd)
		return
	}
	comRel(envDone)
	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			return
		}
		pTranslate.Call(uintptr(unsafe.Pointer(&m)))
		pDispatch.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (w *Window) fail(err error) {
	select {
	case w.ready <- err:
	default:
	}
	pDestroyWindow.Call(w.hwnd)
}

// setup configures the view: settings, the checks on navigation, frames,
// pop-ups, permissions, downloads and certificates.
func (w *Window) setup(ctrl unsafe.Pointer) {
	w.ctrl = ctrl
	var wv unsafe.Pointer
	comCall(ctrl, ctrlGetCoreWebView, uintptr(unsafe.Pointer(&wv)))
	if wv == nil {
		w.fail(errors.New("pane: WebView2 gave no view"))
		return
	}
	w.wv = wv
	var st unsafe.Pointer
	comCall(wv, wvGetSettings, uintptr(unsafe.Pointer(&st)))
	if st != nil {
		comCall(st, setPutDevTools, 0)
		comCall(st, setPutStatusBar, 1)
		comCall(st, setPutHostObjects, 0)
		comCall(st, setPutWebMessage, 0)
		var s3, s4 unsafe.Pointer
		if comCall(st, 0, uintptr(unsafe.Pointer(&iidSettings3)), uintptr(unsafe.Pointer(&s3))) == 0 && s3 != nil {
			comCall(s3, set3PutAccelerators, 0) // no browser shortcuts (print, save, address bar)
			comRelease(s3)
		}
		if comCall(st, 0, uintptr(unsafe.Pointer(&iidSettings4)), uintptr(unsafe.Pointer(&s4))) == 0 && s4 != nil {
			comCall(s4, set4PutPasswordSave, 0)
			comCall(s4, set4PutAutofill, 0)
			comRelease(s4)
		}
		comRelease(st)
	}
	var token int64
	navCheck := func(top bool) func(a uintptr, args unsafe.Pointer) uintptr {
		return func(_ uintptr, ap unsafe.Pointer) uintptr {
			var p *uint16
			var user int32
			comCall(ap, navGetURI, uintptr(unsafe.Pointer(&p)))
			comCall(ap, navGetUserInit, uintptr(unsafe.Pointer(&user)))
			u := coString(p)
			if !w.allowNav(u, top, user != 0) {
				comCall(ap, navPutCancel, 1)
			}
			return 0
		}
	}
	add := func(slot int, iid windows.GUID, fn func(a uintptr, b unsafe.Pointer) uintptr) {
		h := newObj(&vtHandler, iid, fn)
		comCall(wv, slot, h.ptr(), uintptr(unsafe.Pointer(&token)))
		comRel(h)
	}
	add(wvAddNavStarting, iidNavStarting, navCheck(true))
	add(wvAddFrameNavStarting, iidNavStarting, navCheck(false))
	add(wvAddNewWindow, iidNewWindow, func(_ uintptr, ap unsafe.Pointer) uintptr {
		var p *uint16
		comCall(ap, nwGetURI, uintptr(unsafe.Pointer(&p)))
		u := coString(p)
		comCall(ap, nwPutHandled, 1) // never a second window
		if w.allowNav(u, true, false) {
			w.navigate(u)
		}
		return 0
	})
	add(wvAddPermission, iidPermission, func(_ uintptr, args unsafe.Pointer) uintptr {
		comCall(args, permPutState, 2) // DENY: camera, microphone, location, notifications, clipboard
		return 0
	})
	add(wvAddTitleChanged, iidTitleChanged, func(uintptr, unsafe.Pointer) uintptr {
		w.refreshTitle()
		return 0
	})
	var wv4, wv5 unsafe.Pointer
	if comCall(wv, 0, uintptr(unsafe.Pointer(&iidWebView4)), uintptr(unsafe.Pointer(&wv4))) == 0 && wv4 != nil {
		h := newObj(&vtHandler, iidDownload, func(_ uintptr, args unsafe.Pointer) uintptr {
			comCall(args, dlPutCancel, 1) // downloads are refused in this version
			return 0
		})
		comCall(wv4, wv4AddDownloadStarting, h.ptr(), uintptr(unsafe.Pointer(&token)))
		comRel(h)
		comRelease(wv4)
	}
	if comCall(wv, 0, uintptr(unsafe.Pointer(&iidWebView5)), uintptr(unsafe.Pointer(&wv5))) == 0 && wv5 != nil {
		h := newObj(&vtHandler, iidCertRequested, w.onCertificate)
		comCall(wv5, wv5AddCertRequested, h.ptr(), uintptr(unsafe.Pointer(&token)))
		comRel(h)
		comRelease(wv5)
	}
	w.resize()
	comCall(ctrl, ctrlPutIsVisible, 1)
	w.navigate("about:blank")
	select {
	case w.ready <- nil:
	default:
	}
}

func (w *Window) allowNav(u string, top, user bool) bool {
	if u == "about:blank" {
		return true
	}
	if w.o.AllowNavigation == nil {
		return false
	}
	ok := false
	func() {
		defer func() { _ = recover() }()
		ok = w.o.AllowNavigation(u, top, user)
	}()
	return ok
}

// onCertificate asks the host whether the site may ask for the user's
// certificate. Yes leaves WebView2 to show its own picker (and Windows
// its PIN prompt); no cancels the request.
func (w *Window) onCertificate(_ uintptr, ap unsafe.Pointer) uintptr {
	var hp *uint16
	var port int32
	comCall(ap, certGetHost, uintptr(unsafe.Pointer(&hp)))
	comCall(ap, certGetPort, uintptr(unsafe.Pointer(&port)))
	host := coString(hp)
	if w.o.Certificate == nil {
		comCall(ap, certPutCancel, 1)
		return 0
	}
	var def unsafe.Pointer
	if comCall(ap, certGetDeferral, uintptr(unsafe.Pointer(&def))) != 0 || def == nil {
		comCall(ap, certPutCancel, 1)
		return 0
	}
	comCall(ap, 1) // AddRef: kept until the answer
	go func() {
		ok := false
		func() {
			defer func() { _ = recover() }()
			ok = w.o.Certificate(host, int(port))
		}()
		if !w.post(func() {
			if !ok {
				comCall(ap, certPutCancel, 1)
			}
			comCall(def, defComplete)
			comRelease(def)
			comRelease(ap)
		}) {
			return
		}
	}()
	return 0
}

func (w *Window) navigate(u string) {
	p, _ := windows.UTF16PtrFromString(u)
	comCall(w.wv, wvNavigate, uintptr(unsafe.Pointer(p)))
}

func (w *Window) resize() {
	if w.ctrl == nil {
		return
	}
	var r rect
	pGetClientRect.Call(w.hwnd, uintptr(unsafe.Pointer(&r)))
	comCall(w.ctrl, ctrlPutBounds, uintptr(unsafe.Pointer(&r)))
}

func (w *Window) refreshTitle() {
	t := w.o.Title
	if w.wv != nil {
		var p *uint16
		comCall(w.wv, wvGetDocumentTitle, uintptr(unsafe.Pointer(&p)))
		if dt := coString(p); dt != "" {
			if len([]rune(dt)) > 60 {
				dt = string([]rune(dt)[:59]) + "…"
			}
			t += " — " + dt
		}
	}
	if w.status != "" {
		t += " — " + w.status
	}
	tp, _ := windows.UTF16PtrFromString(t)
	pSetWindowText.Call(w.hwnd, uintptr(unsafe.Pointer(tp)))
}

// post runs fn on the window's thread. It's false once the window closed.
func (w *Window) post(fn func()) bool {
	select {
	case <-w.done:
		return false
	default:
	}
	w.qmu.Lock()
	w.queue = append(w.queue, fn)
	w.qmu.Unlock()
	r, _, _ := pPostMessage.Call(w.hwnd, wmRun, 0, 0)
	return r != 0
}

func (w *Window) runQueue() {
	w.qmu.Lock()
	q := w.queue
	w.queue = nil
	w.qmu.Unlock()
	for _, fn := range q {
		func() {
			defer func() { _ = recover() }()
			fn()
		}()
	}
}

func wndProc(hwnd, m, wParam, lParam uintptr) uintptr {
	winMu.Lock()
	w := panes[hwnd]
	winMu.Unlock()
	if w == nil {
		r, _, _ := pDefWindowProc.Call(hwnd, m, wParam, lParam)
		return r
	}
	switch uint32(m) {
	case wmRun:
		w.runQueue()
		return 0
	case 0x0005: // WM_SIZE
		w.resize()
		return 0
	case 0x0003: // WM_MOVE
		if w.ctrl != nil {
			comCall(w.ctrl, ctrlNotifyMoved)
		}
	case 0x0007: // WM_SETFOCUS
		if w.ctrl != nil {
			comCall(w.ctrl, ctrlMoveFocus, 0)
		}
		return 0
	case 0x0010: // WM_CLOSE
		w.closing.Store(true)
		if w.ctrl != nil {
			comCall(w.ctrl, ctrlClose)
			comRelease(w.ctrl)
			w.ctrl = nil
		}
		pDestroyWindow.Call(hwnd)
		return 0
	case 0x0002: // WM_DESTROY
		winMu.Lock()
		delete(panes, hwnd)
		winMu.Unlock()
		pPostQuit.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, m, wParam, lParam)
	return r
}

func (w *Window) finish() {
	w.once.Do(func() {
		select {
		case w.ready <- errors.New("pane: the window closed"):
		default:
		}
		close(w.done)
		w.evq.Stop()
		if w.o.OnClosed != nil {
			go w.o.OnClosed()
		}
	})
}

// Show brings the window up without taking the keyboard from the user, so
// it keeps drawing while the agent works.
func (w *Window) Show() {
	w.post(func() {
		pShowWindow.Call(w.hwnd, 4) // SW_SHOWNOACTIVATE (restores a minimized window)
		if w.ctrl != nil {
			comCall(w.ctrl, ctrlPutIsVisible, 1)
		}
	})
}

// SetStatus puts a short status after the title ("The assistant is
// working").
func (w *Window) SetStatus(s string) {
	w.post(func() {
		w.status = s
		w.refreshTitle()
	})
}

// Close closes the window.
func (w *Window) Close() error {
	if !w.post(func() {
		pPostMessage.Call(w.hwnd, 0x0010, 0, 0) // WM_CLOSE
	}) {
		return nil
	}
	<-w.done
	return nil
}

// Done is closed when the window has closed.
func (w *Window) Done() <-chan struct{} { return w.done }

// Transport is the window's DevTools transport.
func (w *Window) Transport() cdp.Transport { return (*transport)(w) }

// transport is the DevTools Transport over WebView2's own API.
type transport Window

func (t *transport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	w := (*Window)(t)
	pj := "{}"
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		pj = string(b)
	}
	type res struct {
		code uint32
		body string
	}
	ch := make(chan res, 1)
	if !w.post(func() {
		if w.wv == nil {
			ch <- res{0x80004005, `{"message":"the window has closed"}`}
			return
		}
		mp, _ := windows.UTF16PtrFromString(method)
		pp, _ := windows.UTF16PtrFromString(pj)
		h := newObj(&vtHandler, iidCDPCompleted, func(code uintptr, body unsafe.Pointer) uintptr {
			s := ""
			if body != nil {
				s = windows.UTF16PtrToString((*uint16)(body))
			}
			select {
			case ch <- res{uint32(code), s}:
			default:
			}
			return 0
		})
		if hr := comCall(w.wv, wvCallDevTools, uintptr(unsafe.Pointer(mp)), uintptr(unsafe.Pointer(pp)), h.ptr()); hr != 0 {
			select {
			case ch <- res{uint32(hr), ""}:
			default:
			}
		}
		comRel(h)
		runtime.KeepAlive(mp)
		runtime.KeepAlive(pp)
	}) {
		return nil, cdp.ErrClosed
	}
	select {
	case r := <-ch:
		if r.code != 0 {
			var ce cdp.Error
			if json.Unmarshal([]byte(r.body), &ce) == nil && ce.Message != "" {
				return nil, &ce
			}
			return nil, fmt.Errorf("pane: %s failed (0x%x)", method, r.code)
		}
		if r.body == "" {
			return json.RawMessage("{}"), nil
		}
		return json.RawMessage(r.body), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.done:
		return nil, cdp.ErrClosed
	}
}

func (t *transport) Subscribe(event string, fn func(json.RawMessage)) func() {
	w := (*Window)(t)
	w.smu.Lock()
	w.subID++
	id := w.subID
	if w.subs[event] == nil {
		w.subs[event] = map[int]func(json.RawMessage){}
	}
	w.subs[event][id] = fn
	need := !w.receivers[event]
	w.receivers[event] = true
	w.smu.Unlock()
	if need {
		w.post(func() {
			if w.wv == nil {
				return
			}
			ep, _ := windows.UTF16PtrFromString(event)
			var recv unsafe.Pointer
			if comCall(w.wv, wvGetDevToolsReceiver, uintptr(unsafe.Pointer(ep)), uintptr(unsafe.Pointer(&recv))) != 0 || recv == nil {
				return
			}
			h := newObj(&vtHandler, iidCDPEvent, func(_ uintptr, args unsafe.Pointer) uintptr {
				var p *uint16
				comCall(args, evArgsGetJSON, uintptr(unsafe.Pointer(&p)))
				body := json.RawMessage(coString(p))
				w.smu.Lock()
				var fns []func(json.RawMessage)
				for _, f := range w.subs[event] {
					fns = append(fns, f)
				}
				w.smu.Unlock()
				for _, f := range fns {
					f := f
					w.evq.Push(func() { f(body) })
				}
				return 0
			})
			var token int64
			comCall(recv, recvAdd, h.ptr(), uintptr(unsafe.Pointer(&token)))
			comRel(h)
			runtime.KeepAlive(ep)
			// The receiver stays registered for the window's life; it's
			// released with the view.
		})
	}
	return func() {
		w.smu.Lock()
		delete(w.subs[event], id)
		w.smu.Unlock()
	}
}

func (t *transport) Close() error { return nil } // the window's owner closes it
