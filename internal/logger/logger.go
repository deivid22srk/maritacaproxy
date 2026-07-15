// Package logger provides a minimal structured logger used throughout the proxy.
package logger

import (
	"fmt"
	"os"
	"time"
)

var (
	debugMode = os.Getenv("DEBUG") == "true"
)

// Info logs informational messages with [INFO] prefix.
func Info(format string, args ...any) {
	log("INFO", format, args...)
}

// Warn logs warning messages with [WARN] prefix.
func Warn(format string, args ...any) {
	log("WARN", format, args...)
}

// Error logs error messages with [ERROR] prefix.
func Error(format string, args ...any) {
	log("ERROR", format, args...)
}

// Debug logs debug messages when DEBUG=true.
func Debug(format string, args ...any) {
	if debugMode {
		log("DEBUG", format, args...)
	}
}

func log(level, format string, args ...any) {
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "%s [%s] %s\n", ts, level, msg)
}
