//go:build windows

// testapp is a small Win32 window for the desktop driver's checks: a text
// box, a checkbox and a "Send report" button that changes the title.
package main

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32         = windows.NewLazySystemDLL("user32.dll")
	kernel32       = windows.NewLazySystemDLL("kernel32.dll")
	pRegisterClass = user32.NewProc("RegisterClassExW")
	pCreateWindow  = user32.NewProc("CreateWindowExW")
	pDefProc       = user32.NewProc("DefWindowProcW")
	pShow          = user32.NewProc("ShowWindow")
	pGetMessage    = user32.NewProc("GetMessageW")
	pTranslate     = user32.NewProc("TranslateMessage")
	pDispatch      = user32.NewProc("DispatchMessageW")
	pPostQuit      = user32.NewProc("PostQuitMessage")
	pSetText       = user32.NewProc("SetWindowTextW")
	pModule        = kernel32.NewProc("GetModuleHandleW")
)

type wndClassEx struct {
	cbSize, style             uint32
	proc                      uintptr
	clsExtra, wndExtra        int32
	inst, icon, cursor, brush uintptr
	menu, class               *uint16
	iconSm                    uintptr
}

type msg struct {
	hwnd           uintptr
	message        uint32
	wParam, lParam uintptr
	time           uint32
	pt             [2]int32
	private        uint32
}

var main_ uintptr

func u(s string) uintptr { p, _ := windows.UTF16PtrFromString(s); return uintptr(unsafe.Pointer(p)) }

func proc(h, m, w, l uintptr) uintptr {
	switch m {
	case 0x0111: // WM_COMMAND
		if w&0xFFFF == 101 {
			pSetText.Call(main_, u("gohelm test app - sent"))
		}
		return 0
	case 0x0002:
		pPostQuit.Call(0)
		return 0
	}
	r, _, _ := pDefProc.Call(h, m, w, l)
	return r
}

func main() {
	runtime.LockOSThread()
	inst, _, _ := pModule.Call(0)
	cls, _ := windows.UTF16PtrFromString("gohelmTestApp")
	wc := wndClassEx{proc: windows.NewCallback(proc), inst: inst, brush: 6, class: cls}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	pRegisterClass.Call(uintptr(unsafe.Pointer(&wc)))
	main_, _, _ = pCreateWindow.Call(0, u("gohelmTestApp"), u("gohelm test app"), 0x00CF0000|0x10000000, 100, 100, 460, 260, 0, 0, inst, 0)
	ws := uintptr(0x50000000) // WS_CHILD | WS_VISIBLE
	st, _, _ := pCreateWindow.Call(0, u("STATIC"), u("Report notes"), ws, 20, 20, 120, 20, main_, 0, inst, 0)
	_ = st
	pCreateWindow.Call(0x200, u("EDIT"), u(""), ws|0x00010000|0x0080, 140, 18, 280, 24, main_, 100, inst, 0)
	pCreateWindow.Call(0, u("BUTTON"), u("Remember me"), ws|0x00010000|0x3, 20, 60, 200, 24, main_, 102, inst, 0)
	pCreateWindow.Call(0, u("BUTTON"), u("Send report"), ws|0x00010000, 20, 100, 140, 30, main_, 101, inst, 0)
	pShow.Call(main_, 5)
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
