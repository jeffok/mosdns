// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// The kernel owns this lock until the process actually exits; it is not a PID
// file and a stale on-disk lock pathname is harmless after a crash.
func acquireGuardProcessLock(stateFile string) (*os.File, error) {
	if stateFile == "" {
		return nil, fmt.Errorf("guard state file is required")
	}
	name := stateFile + ".lock"
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("guard agent already running: %w", err)
	}
	return f, nil
}
