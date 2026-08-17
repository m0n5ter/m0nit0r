//go:build !windows

package main

import "context"

// inServiceMode is Windows-specific; elsewhere systemd runs the process in the
// foreground and signals it normally.
func inServiceMode() bool { return false }

func runService(run func(context.Context) error) error { return nil }
