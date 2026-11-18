//go:build windows

package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
)

//go:embed worker.ps1
var workerScript []byte

// Worker is a hidden powershell.exe that answers one JSON request per line.
type Worker struct {
	name string

	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan []byte
}

func NewWorker(name string) *Worker { return &Worker{name: name} }

// encodedScript is the worker script as -EncodedCommand expects it (base64 of UTF-16LE).
// Passing it inline means PowerShell's execution policy doesn't apply and nothing is written to disk.
func encodedScript() string {
	u := utf16.Encode([]rune(string(workerScript)))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		b[2*i] = byte(c)
		b[2*i+1] = byte(c >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func workerScriptPath() (string, error) {
	p := filepath.Join(dataDir(), "worker.ps1")
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, workerScript) {
		return p, nil
	}
	return p, os.WriteFile(p, workerScript, 0o600)
}

// start runs the script from a file; if that's blocked (e.g. by an execution-policy GPO) it passes the script inline.
func (w *Worker) start() error {
	err := w.startWith(false)
	if err != nil {
		logf("[%s] %v - retrying with the script passed inline", w.name, err)
		err = w.startWith(true)
	}
	return err
}

func (w *Worker) startWith(inline bool) error {
	ps := filepath.Join(os.Getenv("SystemRoot"), `System32\WindowsPowerShell\v1.0\powershell.exe`)
	args := []string{"-NoLogo", "-NoProfile", "-NonInteractive"}
	if inline {
		args = append(args, "-EncodedCommand", encodedScript())
	} else {
		script, err := workerScriptPath()
		if err != nil {
			return fmt.Errorf("can't write helper script: %w", err)
		}
		args = append(args, "-ExecutionPolicy", "Bypass", "-File", script)
	}
	cmd := exec.Command(ps, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("can't start PowerShell: %w", err)
	}
	lines := make(chan []byte, 4)
	go func() {
		r := bufio.NewReaderSize(stdout, 64*1024)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				line = bytes.TrimSpace(line)
				if bytes.HasPrefix(line, []byte("@@VH ")) {
					lines <- line[5:]
				} else if len(line) > 0 {
					logf("[%s] %s", w.name, line)
				}
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	go func() {
		s := bufio.NewScanner(stderr)
		for s.Scan() {
			logf("[%s stderr] %s", w.name, s.Text())
		}
	}()
	go func() { _ = cmd.Wait() }()
	w.cmd, w.stdin, w.lines = cmd, stdin, lines

	// Wait for the "ready" line (UIA/CIM assemblies load on first use, so this is quick).
	select {
	case _, ok := <-lines:
		if !ok {
			w.kill()
			return errors.New("PowerShell helper exited at start-up (see log)")
		}
	case <-time.After(20 * time.Second):
		w.kill()
		return errors.New("PowerShell helper didn't start in time")
	}
	return nil
}

func (w *Worker) kill() {
	if w.stdin != nil {
		_ = w.stdin.Close()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	w.cmd, w.stdin, w.lines = nil, nil, nil
}

// Stop ends the PowerShell process.
func (w *Worker) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.kill()
}

// Warm starts the process ahead of the first request.
func (w *Worker) Warm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd == nil {
		if err := w.start(); err != nil {
			logf("[%s] %v", w.name, err)
		}
	}
}

// Call sends a request and decodes the reply into out. The process is restarted if it hangs or dies.
func (w *Worker) Call(req any, out any, timeout time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd == nil {
		if err := w.start(); err != nil {
			return err
		}
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := w.stdin.Write(append(b, '\n')); err != nil {
		w.kill()
		return fmt.Errorf("PowerShell helper stopped: %w", err)
	}
	select {
	case line, ok := <-w.lines:
		if !ok {
			w.kill()
			return errors.New("PowerShell helper stopped unexpectedly")
		}
		if err := json.Unmarshal(line, out); err != nil {
			return fmt.Errorf("bad reply from PowerShell helper: %v (%s)", err, truncate(string(line), 200))
		}
		return nil
	case <-time.After(timeout):
		w.kill()
		return fmt.Errorf("no answer within %s", timeout)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---- typed requests

type uiaReply struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message"`
	Names   []string `json:"names"`
	Process string   `json:"process"`
}

func (w *Worker) NamesAt(x, y int32) (uiaReply, error) {
	var r uiaReply
	err := w.Call(map[string]any{"op": "uia", "x": x, "y": y}, &r, 8*time.Second)
	if err == nil && !r.OK {
		err = errors.New(r.Message)
	}
	return r, err
}

type Proc struct {
	Name  string  `json:"name"`
	PID   int     `json:"pid"`
	CPU   float64 `json:"cpu"`
	MemMB float64 `json:"memMB"`
}

type guestReply struct {
	OK           bool    `json:"ok"`
	Message      string  `json:"message"`
	Target       string  `json:"target"`
	LogicalProcs int     `json:"logicalProcs"`
	CPU          float64 `json:"cpu"`
	Mem          float64 `json:"mem"`
	Net          float64 `json:"net"`
	TotalMB      float64 `json:"totalMB"`
	UsedMB       float64 `json:"usedMB"`
	ProcessCount int     `json:"processCount"`
	Top          []Proc  `json:"top"`
}

func (w *Worker) Guest(key string, targets []string, user, pass string) (guestReply, error) {
	var r guestReply
	if targets == nil {
		targets = []string{}
	}
	err := w.Call(map[string]any{"op": "guest", "key": key + "|" + strings.ToLower(user), "targets": targets, "user": user, "pass": pass}, &r, 60*time.Second)
	if err == nil && !r.OK {
		err = errors.New(r.Message)
	}
	return r, err
}
