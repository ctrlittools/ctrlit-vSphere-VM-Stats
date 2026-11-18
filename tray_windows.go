//go:build windows

package main

import (
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const wmTray = wmApp + 1

type tray struct {
	a              *App
	hwnd           uintptr
	mu             sync.Mutex
	nid            notifyIconData
	taskbarCreated uintptr
}

var theTray *tray

// newTray must run on the UI thread: the tray's messages are handled by that thread's message loop.
func newTray(a *App, icon uintptr) *tray {
	t := &tray{a: a}
	theTray = t
	cls := utf16p("CtrlitVMStatsTray")
	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   syscall.NewCallback(trayProc),
		HInstance:     moduleHandle(),
		LpszClassName: cls,
	}
	_, _, _ = pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	t.hwnd, _, _ = pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(cls)), 0,
		0, 0, 0, 0, 0, 0, moduleHandle(), 0)
	t.taskbarCreated, _, _ = pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(utf16p("TaskbarCreated"))))

	t.nid.CbSize = uint32(unsafe.Sizeof(notifyIconData{}))
	t.nid.HWnd = t.hwnd
	t.nid.UID = 1
	t.nid.UCallbackMessage = wmTray
	t.nid.HIcon = icon
	copyUTF16(t.nid.SzTip[:], "Ctrlit vSphere VM Stats – starting…")
	t.add()
	return t
}

func copyUTF16(dst []uint16, s string) {
	u, _ := windows.UTF16FromString(s)
	if len(u) > len(dst) {
		u = append(u[:len(dst)-1], 0)
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst, u)
}

func (t *tray) add() {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.nid
	n.UFlags = nifMessage | nifIcon | nifTip
	_, _, _ = pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&n)))
}

func (t *tray) remove() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.nid
	_, _, _ = pShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&n)))
}

func (t *tray) setTip(s string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	copyUTF16(t.nid.SzTip[:], s)
	n := t.nid
	n.UFlags = nifTip
	_, _, _ = pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

// notify shows a Windows notification from the tray icon.
func (t *tray) notify(title, msg string, isError bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.nid
	n.UFlags = nifInfo
	copyUTF16(n.SzInfoTitle[:], title)
	copyUTF16(n.SzInfo[:], msg)
	n.DwInfoFlags = niifInfo
	if isError {
		n.DwInfoFlags = niifError
	}
	_, _, _ = pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

const (
	cmdSettings = 1 + iota
	cmdAbout
	cmdStartup
	cmdLog
	cmdExit
)

func (t *tray) showMenu() {
	m, _, _ := pCreatePopupMenu.Call()
	add := func(flags, id uintptr, text string) {
		_, _, _ = pAppendMenuW.Call(m, flags, id, uintptr(unsafe.Pointer(utf16p(text))))
	}
	add(mfString|mfGrayed, 0, appName+" "+appVersion)
	add(mfSeparator, 0, "")
	add(mfString, cmdSettings, "Settings…")
	startup := uintptr(mfString)
	if startupEnabled() {
		startup |= mfChecked
	}
	add(startup, cmdStartup, "Start with Windows")
	add(mfString, cmdLog, "Open log")
	add(mfString, cmdAbout, "About")
	add(mfSeparator, 0, "")
	add(mfString, cmdExit, "Exit")

	p := cursorPos()
	_, _, _ = pSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(m, tpmRightButton|tpmReturnCmd|tpmNoNotify|tpmBottomAlign,
		uintptr(p.X), uintptr(p.Y), 0, t.hwnd, 0)
	_, _, _ = pPostMessageW.Call(t.hwnd, wmNull, 0, 0)
	_, _, _ = pDestroyMenu.Call(m)

	switch cmd {
	case cmdSettings:
		t.a.openSettings()
	case cmdStartup:
		if err := setStartup(!startupEnabled()); err != nil {
			logf("start-up setting: %v", err)
		}
	case cmdLog:
		shellOpen(logPath())
	case cmdAbout:
		t.a.openAbout()
	case cmdExit:
		t.a.w.Terminate()
	}
}

func trayProc(hwnd, msg, wp, lp uintptr) uintptr {
	if t := theTray; t != nil {
		switch {
		case msg == wmTray:
			switch lp & 0xFFFF {
			case wmLButtonUp:
				t.a.openSettings()
				return 0
			case wmRButtonUp:
				t.showMenu()
				return 0
			}
		case t.taskbarCreated != 0 && msg == t.taskbarCreated: // Explorer restarted
			t.add()
		}
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}
