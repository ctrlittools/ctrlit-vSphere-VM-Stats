//go:build !windows

package main

import "log"

func logf(format string, args ...any) { log.Printf(format, args...) }
