//go:build !linux

package main

// The Go memory watchdog is the only limit on these platforms.
func boundWorker() error { return nil }
