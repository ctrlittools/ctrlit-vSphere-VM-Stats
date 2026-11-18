//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	logMu   sync.Mutex
	logFile *os.File
)

func logPath() string { return filepath.Join(dataDir(), "ctrlit-vSphere-VM-Stats.log") }

func initLog() {
	p := logPath()
	if fi, err := os.Stat(p); err == nil && fi.Size() > 1<<20 {
		_ = os.Rename(p, p+".old")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		logFile = f
	}
}

func logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile == nil {
		return
	}
	fmt.Fprintf(logFile, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}
