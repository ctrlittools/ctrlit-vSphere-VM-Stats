//go:build windows

package main

import (
	_ "embed"
	"path/filepath"
	"runtime"
	"syscall"

	webview2 "github.com/jchv/go-webview2"
)

//go:embed app.ico
var appIcon []byte

func init() { runtime.LockOSThread() } // the window and its message loop live on the main thread

var (
	theApp      *App
	origWndProc uintptr
)

// subclassProc keeps the window alive when it's closed (it's reused) and stops the popup taking focus.
func subclassProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmClose:
		if theApp != nil {
			theApp.hide()
		}
		return 0
	case wmMouseActivate:
		if theApp != nil && theApp.getMode() == "popup" {
			return maNoActivate
		}
	}
	r, _, _ := pCallWindowProcW.Call(origWndProc, hwnd, msg, wp, lp)
	return r
}

func main() {
	setDPIAware()
	initLog()
	logf("Ctrlit vSphere VM Stats %s starting", appVersion)

	if !singleInstance() {
		messageBox("Ctrlit vSphere VM Stats is already running.\n\nLook for its icon in the notification area (system tray), next to the clock.",
			"Ctrlit vSphere VM Stats", mbIconInfo)
		return
	}

	a := newApp()
	theApp = a
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  filepath.Join(dataDir(), "WebView2"),
		WindowOptions: webview2.WindowOptions{
			Title:  appName,
			Width:  600,
			Height: 500,
			Center: true,
		},
	})
	if w == nil {
		logf("WebView2 runtime not available")
		messageBox("Ctrlit vSphere VM Stats needs the Microsoft Edge WebView2 Runtime, which isn't installed on this PC.\n\n"+
			"Click OK to open Microsoft's download page. Install the \"Evergreen Bootstrapper\", then start Ctrlit vSphere VM Stats again.",
			"Ctrlit vSphere VM Stats", mbIconError)
		shellOpen("https://developer.microsoft.com/microsoft-edge/webview2/")
		return
	}
	a.w = w
	a.hwnd = uintptr(w.Window())
	_, _, _ = pShowWindow.Call(a.hwnd, swHide)

	big := iconFromICO(appIcon, 32)
	small := iconFromICO(appIcon, smallIconSize())
	_, _, _ = pSendMessageW.Call(a.hwnd, 0x0080, 1, big)   // WM_SETICON, ICON_BIG
	_, _, _ = pSendMessageW.Call(a.hwnd, 0x0080, 0, small) // ICON_SMALL

	origWndProc = setLongPtr(a.hwnd, gwlpWndProc, syscall.NewCallback(subclassProc))
	a.setMode("popup")
	a.mode = "hidden"
	roundCorners(a.hwnd)

	if err := w.Bind("vhCmd", a.onCmd); err != nil {
		logf("bind: %v", err)
	}
	w.SetHtml(uiHTML)

	a.tray = newTray(a, small)
	go a.keyLoop()
	go a.startup()

	w.Run()
	a.shutdown()
	logf("stopped")
}
