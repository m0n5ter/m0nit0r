//go:build !linux

package main

// notifyReady is a systemd concept; there is nothing to signal elsewhere.
func notifyReady() {}
