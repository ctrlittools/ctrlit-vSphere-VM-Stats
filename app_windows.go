//go:build windows

package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	webview2 "github.com/jchv/go-webview2"
)

//go:embed ui.html
var uiHTML string

//go:embed about_icon.png
var aboutIcon []byte

const (
	appName    = "Ctrlit vSphere VM Stats"
	appVersion = "1.0.0"
	appAuthor  = "ctrlittools"
	repoURL    = "https://github.com/ctrlittools/ctrlit-vSphere-VM-Stats"
	latestURL  = repoURL + "/releases/latest"
	supportURL = "https://ko-fi.com/ctrlittools"
)

func isDialog(mode string) bool { return mode == "settings" || mode == "about" }

// Config is saved as JSON in %APPDATA%\ctrlit-vSphere-VM-Stats; passwords are DPAPI-encrypted for this Windows user.
type Config struct {
	Server        string `json:"server"`
	User          string `json:"user"`
	Password      string `json:"password"`
	Insecure      bool   `json:"insecure"`
	GuestMode     string `json:"guestMode"` // "current" or "account"
	GuestUser     string `json:"guestUser"`
	GuestPassword string `json:"guestPassword"`
	Welcomed      bool   `json:"welcomed"`
}

func configPath() string { return filepath.Join(dataDir(), "config.json") }

func loadConfig() Config {
	var c Config
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), b, 0o600)
}

func (c Config) vcConfig() VCConfig {
	return VCConfig{Server: c.Server, User: c.User, Password: unprotect(c.Password), Insecure: c.Insecure}
}

type guestPoint struct {
	T   int64   `json:"t"`
	CPU float64 `json:"cpu"`
	Mem float64 `json:"mem"`
	Net float64 `json:"net"`
}

type guestState struct {
	Target       string       `json:"target"`
	LogicalProcs int          `json:"logicalProcs"`
	TotalMB      float64      `json:"totalMB"`
	UsedMB       float64      `json:"usedMB"`
	ProcessCount int          `json:"processCount"`
	History      []guestPoint `json:"history"`
	Top          []Proc       `json:"top"`
}

type guestErr struct {
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

type popupState struct {
	Name       string        `json:"name"`
	VM         *VMInfo       `json:"vm"`
	VS         *VSphereStats `json:"vs"`
	VSError    string        `json:"vsError,omitempty"`
	Guest      *guestState   `json:"guest"`
	GuestError *guestErr     `json:"guestError"`
	Updated    int64         `json:"updated"`
}

type session struct {
	ctx     context.Context
	cancel  context.CancelFunc
	entry   vmEntry
	matches int

	mu    sync.Mutex
	state popupState
	info  *VMInfo
}

type App struct {
	w    webview2.WebView
	hwnd uintptr
	tray *tray

	mu      sync.Mutex
	cfg     Config
	vc      *VC
	sess    *session
	mode    string // "hidden", "popup", "settings"
	pending *point // show the window at this point once the page reports its size

	ui    *Worker
	guest *Worker

	histMu sync.Mutex
	hist   map[string][]guestPoint

	triggering  atomic.Bool
	dragging    atomic.Bool
	lastVCAlert time.Time
}

func newApp() *App {
	a := &App{ui: NewWorker("ui"), guest: NewWorker("guest"), hist: map[string][]guestPoint{}, mode: "hidden"}
	a.cfg = loadConfig()
	if a.cfg.Server != "" {
		a.vc = NewVC(a.cfg.vcConfig())
	}
	return a
}

func (a *App) getMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

func (a *App) currentVC() *VC {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.vc
}

// ---------------------------------------------------------------- start-up

func (a *App) startup() {
	go func() {
		a.ui.Warm()
		_, _ = a.ui.NamesAt(0, 0) // loads UI Automation ahead of the first Ctrl+Shift
	}()
	go a.guest.Warm()

	vc := a.currentVC()
	if vc == nil {
		a.w.Dispatch(a.openSettings)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	n, err := vc.RefreshNames(ctx)
	if err != nil {
		logf("vCenter: %v", err)
		a.tray.setTip("Ctrlit vSphere VM Stats – can't reach vCenter")
		a.tray.notify("Can't connect to vCenter", err.Error()+"\nRight-click the tray icon and choose Settings.", true)
		return
	}
	logf("vCenter %s: %d VM names loaded", a.cfg.Server, n)
	a.tray.setTip("Ctrlit vSphere VM Stats – hover a VM, press Ctrl+Shift")
	a.mu.Lock()
	welcomed := a.cfg.Welcomed
	a.cfg.Welcomed = true
	cfg := a.cfg
	a.mu.Unlock()
	if !welcomed {
		_ = saveConfig(cfg)
		a.tray.notify("Ctrlit vSphere VM Stats is running", "Hover over a VM's name and press Ctrl+Shift. Settings are on the tray icon.", false)
	}
}

func (a *App) shutdown() {
	a.closeSession()
	a.ui.Stop()
	a.guest.Stop()
	if vc := a.currentVC(); vc != nil {
		vc.Close()
	}
	a.tray.remove()
}

// ---------------------------------------------------------------- keyboard & mouse watch

func (a *App) keyLoop() {
	var wasCombo, wasEsc, wasBtn bool
	t := time.NewTicker(35 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		combo := keyDown(vkControl) && keyDown(vkShift) && !keyDown(vkMenu) && !keyDown(vkLWin) && !keyDown(vkRWin)
		if combo && !wasCombo {
			go a.trigger()
		}
		wasCombo = combo

		mode := a.getMode()
		esc := keyDown(vkEscape)
		if esc && !wasEsc && mode == "popup" {
			a.w.Dispatch(a.hide)
		}
		wasEsc = esc

		btn := keyDown(vkLButton) || keyDown(vkRButton)
		if btn && !wasBtn && mode == "popup" && !a.dragging.Load() {
			if !windowRect(a.hwnd).contains(cursorPos()) {
				a.w.Dispatch(a.hide)
			}
		}
		wasBtn = btn
	}
}

var browsers = map[string]bool{"msedge": true, "chrome": true, "firefox": true, "brave": true, "opera": true, "vivaldi": true, "iexplore": true}

func (a *App) trigger() {
	if !a.triggering.CompareAndSwap(false, true) {
		return
	}
	defer a.triggering.Store(false)

	mode := a.getMode()
	if isDialog(mode) {
		return
	}
	p := cursorPos()
	if mode == "popup" && windowRect(a.hwnd).contains(p) {
		return
	}
	vc := a.currentVC()
	if vc == nil {
		a.w.Dispatch(a.openSettings)
		return
	}
	r, err := a.ui.NamesAt(p.X, p.Y)
	if err != nil {
		logf("screen lookup failed: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e, n, ok, verr := vc.Match(ctx, r.Names)
	if !ok && verr == nil && browsers[strings.ToLower(r.Process)] {
		// Browsers build their accessibility tree on first request; ask again.
		time.Sleep(400 * time.Millisecond)
		if r2, err := a.ui.NamesAt(p.X, p.Y); err == nil {
			r = r2
			e, n, ok, verr = vc.Match(ctx, r.Names)
		}
	}
	if verr != nil {
		logf("vCenter: %v", verr)
		a.mu.Lock()
		alert := time.Since(a.lastVCAlert) > time.Minute
		if alert {
			a.lastVCAlert = time.Now()
		}
		a.mu.Unlock()
		if alert {
			a.tray.notify("Can't reach vCenter", verr.Error(), true)
		}
		return
	}
	if !ok {
		logf("no VM name under the mouse (%s): %q", r.Process, r.Names)
		return
	}
	a.mu.Lock()
	same := a.sess != nil && a.sess.entry.Ref == e.Ref && a.mode == "popup"
	a.mu.Unlock()
	if same {
		return
	}
	logf("popup for %s", e.Name)
	a.openPopup(e, n, p)
}

// ---------------------------------------------------------------- popup data

func (a *App) closeSession() {
	a.mu.Lock()
	s := a.sess
	a.sess = nil
	a.mu.Unlock()
	if s != nil {
		s.cancel()
	}
}

func (a *App) openPopup(e vmEntry, matches int, at point) {
	a.closeSession()
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{ctx: ctx, cancel: cancel, entry: e, matches: matches}
	s.state.Name = e.Name
	a.mu.Lock()
	a.sess = s
	a.mu.Unlock()
	a.w.Dispatch(func() {
		a.setMode("popup")
		a.mu.Lock()
		a.pending = &at
		a.mu.Unlock()
	})
	a.push(s)
	go a.vsLoop(s)
}

func (a *App) push(s *session) {
	s.mu.Lock()
	b, err := json.Marshal(s.state)
	s.mu.Unlock()
	if err != nil {
		return
	}
	js := "vh.popup(" + string(b) + ")"
	a.w.Dispatch(func() {
		a.mu.Lock()
		cur := a.sess == s
		a.mu.Unlock()
		if cur {
			a.w.Eval(js)
		}
	})
}

func (a *App) vsLoop(s *session) {
	vc := a.currentVC()
	guestStarted := false
	for {
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		info, st, err := vc.Info(ctx, s.entry, s.matches)
		if err == nil && info.PowerState == "poweredOn" {
			err = vc.Stats(ctx, s.entry.Ref, info.NumCPU, &st)
		}
		cancel()
		if s.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		if info.Found {
			s.info = &info
			s.state.VM = &info
		}
		if err != nil {
			s.state.VSError = err.Error()
		} else {
			s.state.VSError = ""
			if info.PowerState != "poweredOn" {
				st.CPU, st.Mem, st.Net, st.Ready = []Point{}, []Point{}, []Point{}, []Point{}
			}
			s.state.VS = &st
		}
		s.state.Updated = time.Now().UnixMilli()
		s.mu.Unlock()
		a.push(s)
		if !guestStarted && info.Found {
			guestStarted = true
			go a.guestLoop(s)
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}
	}
}

const winrmHint = "Windows inside the VM needs WinRM turned on (run Enable-PSRemoting -Force in the guest) and TCP 5985 open from this PC. " +
	"If the VM isn't in your domain, choose \"Use this account\" in Settings, and add its IP address to TrustedHosts on this PC " +
	"(Set-Item WSMan:\\localhost\\Client\\TrustedHosts -Value <ip> -Concatenate)."

func (a *App) guestLoop(s *session) {
	for {
		s.mu.Lock()
		info := *s.info
		s.mu.Unlock()

		var gerr *guestErr
		switch {
		case info.PowerState != "poweredOn":
			gerr = &guestErr{Message: "The VM is not powered on."}
		case info.GuestFamily != "" && info.GuestFamily != "windowsGuest":
			gerr = &guestErr{Message: "The guest OS is " + info.GuestOS + ". Guest stats and processes need Windows."}
		case len(info.Targets) == 0:
			gerr = &guestErr{Message: "VMware Tools isn't reporting a host name or IP address for this VM (Tools: " + info.ToolsRunning + ")."}
		}
		if gerr == nil {
			a.mu.Lock()
			cfg := a.cfg
			a.mu.Unlock()
			user, pass := "", ""
			if cfg.GuestMode == "account" {
				user, pass = cfg.GuestUser, unprotect(cfg.GuestPassword)
			}
			r, err := a.guest.Guest(info.ID, info.Targets, user, pass)
			if s.ctx.Err() != nil {
				return
			}
			if err != nil {
				gerr = &guestErr{Message: "Couldn't reach Windows inside the VM: " + err.Error(), Hint: winrmHint}
			} else {
				hist := a.addHistory(info.ID, guestPoint{T: time.Now().UnixMilli(), CPU: r.CPU, Mem: r.Mem, Net: r.Net})
				s.mu.Lock()
				s.state.Guest = &guestState{Target: r.Target, LogicalProcs: r.LogicalProcs, TotalMB: r.TotalMB, UsedMB: r.UsedMB,
					ProcessCount: r.ProcessCount, History: hist, Top: r.Top}
				s.state.GuestError = nil
				s.mu.Unlock()
			}
		}
		if gerr != nil {
			s.mu.Lock()
			s.state.GuestError = gerr
			s.mu.Unlock()
		}
		s.mu.Lock()
		s.state.Updated = time.Now().UnixMilli()
		s.mu.Unlock()
		a.push(s)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// addHistory keeps up to 6 minutes of guest samples per VM, so reopening a VM's popup keeps its graph.
func (a *App) addHistory(id string, p guestPoint) []guestPoint {
	a.histMu.Lock()
	defer a.histMu.Unlock()
	h := a.hist[id]
	if len(h) > 0 && p.T-h[len(h)-1].T > 2*60*1000 {
		h = nil // too old to join up
	}
	h = append(h, p)
	cut := 0
	for cut < len(h) && p.T-h[cut].T > 6*60*1000 {
		cut++
	}
	h = append([]guestPoint(nil), h[cut:]...)
	a.hist[id] = h
	return h
}

// ---------------------------------------------------------------- window (UI thread only)

func (a *App) setMode(m string) {
	a.mu.Lock()
	a.mode = m
	a.mu.Unlock()
	style := uintptr(wsPopup | wsClipChildren)
	setLongPtr(a.hwnd, gwlStyle, style)
	after := hwndTopmost
	if isDialog(m) {
		setLongPtr(a.hwnd, gwlExStyle, 0x00040000) // WS_EX_APPWINDOW: taskbar button while Settings is open
		after = ^uintptr(1)                        // HWND_NOTOPMOST
	} else {
		setLongPtr(a.hwnd, gwlExStyle, wsExTopmost|wsExToolWindow|wsExNoActivate)
	}
	setWindowPos(a.hwnd, after, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate|swpFrameChanged)
}

func (a *App) hide() {
	a.closeSession()
	_, _, _ = pShowWindow.Call(a.hwnd, swHide)
	a.mu.Lock()
	a.mode = "hidden"
	a.pending = nil
	a.mu.Unlock()
}

func (a *App) openSettings() {
	a.closeSession()
	_, _, _ = pShowWindow.Call(a.hwnd, swHide)
	a.setMode("settings")
	a.mu.Lock()
	c := a.cfg
	p := cursorPos()
	a.pending = &p
	a.mu.Unlock()
	view := map[string]any{
		"version": appVersion,
		"startup": startupEnabled(),
		"config": map[string]any{
			"server": c.Server, "user": c.User, "hasPassword": c.Password != "", "insecure": c.Insecure,
			"guestMode": c.GuestMode, "guestUser": c.GuestUser, "hasGuestPassword": c.GuestPassword != "",
		},
	}
	b, _ := json.Marshal(view)
	a.w.Eval("vh.settings(" + string(b) + ")")
}

func (a *App) openAbout() {
	a.closeSession()
	_, _, _ = pShowWindow.Call(a.hwnd, swHide)
	a.setMode("about")
	a.mu.Lock()
	p := cursorPos()
	a.pending = &p
	a.mu.Unlock()
	b, _ := json.Marshal(map[string]any{
		"name": appName, "version": appVersion, "author": appAuthor,
		"latest": latestURL, "repo": repoURL, "support": supportURL,
		"icon": "data:image/png;base64," + base64.StdEncoding.EncodeToString(aboutIcon),
	})
	a.w.Eval("vh.about(" + string(b) + ")")
}

// applySize sizes the window to the page and, the first time, shows it.
func (a *App) applySize(cssW, cssH float64) {
	a.mu.Lock()
	mode, pending := a.mode, a.pending
	a.pending = nil
	a.mu.Unlock()
	if mode == "hidden" {
		return
	}
	anchor := point{}
	if pending != nil {
		anchor = *pending
	} else {
		r := windowRect(a.hwnd)
		anchor = point{r.Left, r.Top}
	}
	scale := monitorScale(anchor)
	w := int32(cssW*scale + 0.5)
	h := int32(cssH*scale + 0.5)
	work := workAreaAt(anchor)
	margin := int32(8 * scale)
	if maxH := work.Bottom - work.Top - 2*margin; h > maxH {
		h = maxH
	}
	var x, y int32
	switch {
	case pending != nil && isDialog(mode):
		x = work.Left + (work.Right-work.Left-w)/2
		y = work.Top + (work.Bottom-work.Top-h)/2
	case pending != nil:
		x = anchor.X + int32(18*scale)
		y = anchor.Y + int32(14*scale)
	default:
		x, y = anchor.X, anchor.Y
	}
	if x+w > work.Right-margin {
		x = work.Right - margin - w
	}
	if y+h > work.Bottom-margin {
		y = work.Bottom - margin - h
	}
	if x < work.Left+margin {
		x = work.Left + margin
	}
	if y < work.Top+margin {
		y = work.Top + margin
	}
	after := hwndTopmost
	flags := uintptr(swpNoActivate)
	if isDialog(mode) {
		after = 0
		flags = swpNoZOrder
	}
	if pending != nil {
		flags |= swpShowWindow
	}
	setWindowPos(a.hwnd, after, x, y, w, h, flags)
	if pending != nil && isDialog(mode) {
		_, _, _ = pShowWindow.Call(a.hwnd, swShow)
		_, _, _ = pSetForegroundWindow.Call(a.hwnd)
	}
}

func (a *App) dragLoop() {
	if !a.dragging.CompareAndSwap(false, true) {
		return
	}
	defer a.dragging.Store(false)
	start := cursorPos()
	r := windowRect(a.hwnd)
	for keyDown(vkLButton) || keyDown(vkRButton) {
		p := cursorPos()
		setWindowPos(a.hwnd, 0, r.Left+p.X-start.X, r.Top+p.Y-start.Y, 0, 0, swpNoSize|swpNoZOrder|swpNoActivate)
		time.Sleep(8 * time.Millisecond)
	}
}

// onCmd receives messages from the page (runs on the UI thread).
func (a *App) onCmd(c map[string]any) any {
	op, _ := c["op"].(string)
	switch op {
	case "size":
		w, _ := c["w"].(float64)
		h, _ := c["h"].(float64)
		if w > 0 && h > 0 {
			a.applySize(w, h)
		}
	case "close":
		a.hide()
	case "drag":
		go a.dragLoop()
	case "openLog":
		shellOpen(logPath())
	case "about":
		a.openAbout()
	case "openUrl":
		if u, _ := c["url"].(string); u == latestURL || u == repoURL || u == supportURL {
			shellOpen(u)
		}
	case "save":
		go a.saveSettings(c)
	}
	return nil
}

func (a *App) saveSettings(c map[string]any) {
	str := func(k string) string { s, _ := c[k].(string); return strings.TrimSpace(s) }
	raw := func(k string) string { s, _ := c[k].(string); return s }
	b := func(k string) bool { v, _ := c[k].(bool); return v }

	a.mu.Lock()
	old := a.cfg
	a.mu.Unlock()
	n := old
	n.Server, n.User, n.Insecure = str("server"), str("user"), b("insecure")
	if p := raw("password"); p != "" {
		n.Password = protect(p)
	}
	n.GuestMode = str("guestMode")
	n.GuestUser = str("guestUser")
	if p := raw("guestPassword"); p != "" {
		n.GuestPassword = protect(p)
	}

	result := func(ok bool, msg string) {
		bb, _ := json.Marshal(map[string]any{"ok": ok, "message": msg})
		a.w.Dispatch(func() { a.w.Eval("vh.saveResult(" + string(bb) + ")") })
	}
	if n.Password == "" {
		result(false, "Enter the vCenter password.")
		return
	}
	if n.GuestMode == "account" && (n.GuestUser == "" || n.GuestPassword == "") {
		result(false, "Enter the user name and password for Windows inside the VMs, or choose \"Use my Windows login\".")
		return
	}
	vc := NewVC(n.vcConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	count, err := vc.RefreshNames(ctx)
	if err != nil {
		result(false, "Couldn't connect: "+err.Error())
		return
	}
	if err := setStartup(b("startup")); err != nil {
		logf("start-up setting: %v", err)
	}
	n.Welcomed = true
	if err := saveConfig(n); err != nil {
		result(false, "Connected, but couldn't save settings: "+err.Error())
		return
	}
	a.mu.Lock()
	oldVC := a.vc
	a.cfg, a.vc = n, vc
	a.mu.Unlock()
	if oldVC != nil {
		go oldVC.Close()
	}
	logf("settings saved; vCenter %s: %d VMs", n.Server, count)
	a.tray.setTip("Ctrlit vSphere VM Stats – hover a VM, press Ctrl+Shift")
	result(true, "Connected – "+itoa(count)+" VMs found. Hover over a VM's name and press Ctrl+Shift.")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
