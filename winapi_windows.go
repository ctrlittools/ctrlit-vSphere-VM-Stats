//go:build windows

package main

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")

	pGetAsyncKeyState              = user32.NewProc("GetAsyncKeyState")
	pGetCursorPos                  = user32.NewProc("GetCursorPos")
	pSetWindowPos                  = user32.NewProc("SetWindowPos")
	pShowWindow                    = user32.NewProc("ShowWindow")
	pIsWindowVisible               = user32.NewProc("IsWindowVisible")
	pGetWindowRect                 = user32.NewProc("GetWindowRect")
	pGetWindowLongPtrW             = user32.NewProc("GetWindowLongPtrW")
	pSetWindowLongPtrW             = user32.NewProc("SetWindowLongPtrW")
	pCallWindowProcW               = user32.NewProc("CallWindowProcW")
	pDefWindowProcW                = user32.NewProc("DefWindowProcW")
	pRegisterClassExW              = user32.NewProc("RegisterClassExW")
	pCreateWindowExW               = user32.NewProc("CreateWindowExW")
	pMonitorFromPoint              = user32.NewProc("MonitorFromPoint")
	pGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	pGetDpiForWindow               = user32.NewProc("GetDpiForWindow")
	pSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	pSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	pMessageBoxW                   = user32.NewProc("MessageBoxW")
	pCreatePopupMenu               = user32.NewProc("CreatePopupMenu")
	pAppendMenuW                   = user32.NewProc("AppendMenuW")
	pTrackPopupMenu                = user32.NewProc("TrackPopupMenu")
	pDestroyMenu                   = user32.NewProc("DestroyMenu")
	pPostMessageW                  = user32.NewProc("PostMessageW")
	pRegisterWindowMessageW        = user32.NewProc("RegisterWindowMessageW")
	pCreateIconFromResourceEx      = user32.NewProc("CreateIconFromResourceEx")
	pGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	pShellNotifyIconW              = shell32.NewProc("Shell_NotifyIconW")
	pShellExecuteW                 = shell32.NewProc("ShellExecuteW")
	pCreateMutexW                  = kernel32.NewProc("CreateMutexW")
	pDwmSetWindowAttribute         = dwmapi.NewProc("DwmSetWindowAttribute")
)

const (
	vkLButton = 0x01
	vkRButton = 0x02
	vkShift   = 0x10
	vkControl = 0x11
	vkMenu    = 0x12
	vkEscape  = 0x1B
	vkLWin    = 0x5B
	vkRWin    = 0x5C

	gwlStyle    = -16
	gwlExStyle  = -20
	gwlpWndProc = -4

	wsPopup        = 0x80000000
	wsClipChildren = 0x02000000
	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080
	wsExNoActivate = 0x08000000

	swpNoSize       = 0x0001
	swpNoMove       = 0x0002
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	swpFrameChanged = 0x0020
	swpShowWindow   = 0x0040

	swHide           = 0
	swShowNoActivate = 4
	swShow           = 5

	wmNull          = 0x0000
	wmClose         = 0x0010
	wmMouseActivate = 0x0021
	wmCommand       = 0x0111
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	maNoActivate    = 3

	mfString    = 0x0000
	mfChecked   = 0x0008
	mfSeparator = 0x0800
	mfGrayed    = 0x0001

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmNoNotify    = 0x0080
	tpmBottomAlign = 0x0020

	nimAdd     = 0
	nimModify  = 1
	nimDelete  = 2
	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10
	niifInfo   = 0x1
	niifError  = 0x3

	mbOK            = 0x0
	mbIconError     = 0x10
	mbIconInfo      = 0x40
	mbSetForeground = 0x10000
)

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     uintptr
}

func keyDown(vk int) bool {
	r, _, _ := pGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

func cursorPos() point {
	var p point
	_, _, _ = pGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return p
}

func windowRect(hwnd uintptr) rect {
	var r rect
	_, _, _ = pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

func (r rect) contains(p point) bool {
	return p.X >= r.Left && p.X < r.Right && p.Y >= r.Top && p.Y < r.Bottom
}

func isVisible(hwnd uintptr) bool {
	r, _, _ := pIsWindowVisible.Call(hwnd)
	return r != 0
}

func workAreaAt(p point) rect {
	hmon, _, _ := pMonitorFromPoint.Call(uintptr(*(*uint64)(unsafe.Pointer(&p))), 2) // MONITOR_DEFAULTTONEAREST
	mi := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	_, _, _ = pGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi)))
	return mi.RcWork
}

func dpiScale(hwnd uintptr) float64 {
	if pGetDpiForWindow.Find() != nil {
		return 1
	}
	d, _, _ := pGetDpiForWindow.Call(hwnd)
	if d == 0 {
		return 1
	}
	return float64(d) / 96
}

func setDPIAware() {
	if pSetProcessDpiAwarenessContext.Find() == nil {
		r, _, _ := pSetProcessDpiAwarenessContext.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
		if r != 0 {
			return
		}
	}
	_, _, _ = pSetProcessDPIAware.Call()
}

func setWindowPos(hwnd, after uintptr, x, y, w, h int32, flags uintptr) {
	_, _, _ = pSetWindowPos.Call(hwnd, after, uintptr(x), uintptr(y), uintptr(w), uintptr(h), flags)
}

const hwndTopmost = ^uintptr(0) // (HWND)-1

func getLongPtr(hwnd uintptr, idx int32) uintptr {
	r, _, _ := pGetWindowLongPtrW.Call(hwnd, uintptr(idx))
	return r
}

func setLongPtr(hwnd uintptr, idx int32, v uintptr) uintptr {
	r, _, _ := pSetWindowLongPtrW.Call(hwnd, uintptr(idx), v)
	return r
}

func messageBox(text, title string, flags uintptr) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	_, _, _ = pMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags|mbSetForeground)
}

func shellOpen(target string) {
	verb, _ := windows.UTF16PtrFromString("open")
	t, _ := windows.UTF16PtrFromString(target)
	_, _, _ = pShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(t)), 0, 0, 1)
}

// singleInstance returns false if another copy is already running.
func singleInstance() bool {
	name, _ := windows.UTF16PtrFromString(`Local\ctrlit-vSphere-VM-Stats.SingleInstance`)
	h, _, err := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return true
	}
	return !errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

func roundCorners(hwnd uintptr) {
	if pDwmSetWindowAttribute.Find() != nil {
		return
	}
	pref := uint32(2) // DWMWCP_ROUND (Windows 11; ignored elsewhere)
	_, _, _ = pDwmSetWindowAttribute.Call(hwnd, 33, uintptr(unsafe.Pointer(&pref)), 4)
}

// iconFromICO picks the image closest to size from an .ico file and makes an HICON.
func iconFromICO(ico []byte, size int) uintptr {
	if len(ico) < 6 {
		return 0
	}
	count := int(binary.LittleEndian.Uint16(ico[4:]))
	best, bestDiff := -1, 1<<30
	for i := 0; i < count; i++ {
		e := ico[6+16*i:]
		w := int(e[0])
		if w == 0 {
			w = 256
		}
		d := w - size
		if d < 0 {
			d = -d * 2 // prefer scaling down
		}
		if d < bestDiff {
			best, bestDiff = i, d
		}
	}
	if best < 0 {
		return 0
	}
	e := ico[6+16*best:]
	n := binary.LittleEndian.Uint32(e[8:])
	off := binary.LittleEndian.Uint32(e[12:])
	if int(off+n) > len(ico) {
		return 0
	}
	data := ico[off : off+n]
	h, _, _ := pCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(n), 1, 0x00030000, uintptr(size), uintptr(size), 0)
	return h
}

func smallIconSize() int {
	r, _, _ := pGetSystemMetrics.Call(49) // SM_CXSMICON
	if r == 0 {
		return 16
	}
	return int(r)
}

// ---- data folder, DPAPI, start-up registry

func dataDir() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		base = os.TempDir()
	}
	d := filepath.Join(base, "ctrlit-vSphere-VM-Stats")
	_ = os.MkdirAll(d, 0o700)
	return d
}

func protect(plain string) string {
	if plain == "" {
		return ""
	}
	in := []byte(plain)
	inBlob := windows.DataBlob{Size: uint32(len(in)), Data: &in[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&inBlob, nil, nil, 0, nil, 0x1, &out); err != nil { // CRYPTPROTECT_UI_FORBIDDEN
		return ""
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return base64.StdEncoding.EncodeToString(unsafe.Slice(out.Data, out.Size))
}

func unprotect(enc string) string {
	if enc == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) == 0 {
		return ""
	}
	inBlob := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&inBlob, nil, nil, 0, nil, 0x1, &out); err != nil {
		return ""
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return string(unsafe.Slice(out.Data, out.Size))
}

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "ctrlit-vSphere-VM-Stats"

func startupEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	return err == nil
}

func setStartup(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		err = k.DeleteValue(runValue)
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(runValue, `"`+exe+`"`)
}

var (
	shcore            = windows.NewLazySystemDLL("shcore.dll")
	pGetDpiForMonitor = shcore.NewProc("GetDpiForMonitor")
	pSendMessageW     = user32.NewProc("SendMessageW")
	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

// monitorScale is the display scaling (1.0 = 100%) of the monitor nearest p.
func monitorScale(p point) float64 {
	if pGetDpiForMonitor.Find() != nil {
		return 1
	}
	hmon, _, _ := pMonitorFromPoint.Call(uintptr(*(*uint64)(unsafe.Pointer(&p))), 2)
	var dx, dy uint32
	r, _, _ := pGetDpiForMonitor.Call(hmon, 0, uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy)))
	if r != 0 || dx == 0 {
		return 1
	}
	return float64(dx) / 96
}

func moduleHandle() uintptr {
	h, _, _ := pGetModuleHandleW.Call(0)
	return h
}

func utf16p(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}
