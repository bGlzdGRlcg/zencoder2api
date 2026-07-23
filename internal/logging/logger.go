package logging

// Package logging provides a minimal silent logger for the proxy. Startup
// errors (Fatal/Fatalf) always write to stderr.

import (
	"context"
	"io"
	"log"
	"os"
	"sync"
)

type requestIDContextKey struct{}

// WithRequestID carries the server-generated request ID across package
// boundaries without trusting a client-supplied header.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// RequestIDFromContext returns the server-generated request ID, if present.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

// Level controls which log messages are emitted.
type Level int

const (
	LevelSilent Level = iota
	LevelError
	LevelWarn
	LevelInfo
	LevelDebug
)

var (
	currentLevel Level = LevelSilent
	mu           sync.RWMutex
	logger       = log.New(os.Stderr, "", log.LstdFlags)
)

// Init disables regular application logging. Fatal startup errors remain
// visible through Fatalf.
func Init() {
	mu.Lock()
	defer mu.Unlock()
	currentLevel = LevelSilent
	log.SetOutput(io.Discard)
}

// Enabled reports whether messages at the given level would be emitted.
func Enabled(lvl Level) bool {
	mu.RLock()
	defer mu.RUnlock()
	return lvl <= currentLevel
}

// Errorf emits an error-level message.
func Errorf(format string, args ...interface{}) {
	if Enabled(LevelError) {
		logger.Printf("[ERROR] "+format, args...)
	}
}

// Warnf emits a warning-level message.
func Warnf(format string, args ...interface{}) {
	if Enabled(LevelWarn) {
		logger.Printf("[WARN] "+format, args...)
	}
}

// Infof emits an info-level message.
func Infof(format string, args ...interface{}) {
	if Enabled(LevelInfo) {
		logger.Printf("[INFO] "+format, args...)
	}
}

// Debugf emits a debug-level message.
func Debugf(format string, args ...interface{}) {
	if Enabled(LevelDebug) {
		logger.Printf("[DEBUG] "+format, args...)
	}
}

// Fatalf emits a message and exits. Fatal messages always reach stderr.
func Fatalf(format string, args ...interface{}) {
	logger.SetOutput(os.Stderr)
	logger.Fatalf("[FATAL] "+format, args...)
}
