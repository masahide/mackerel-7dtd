package main

import (
	"golang.org/x/sys/windows"
	"os"
)

func lockUpdateState(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Windows does not support fsync of directory handles. The journal file is synced.
func syncUpdateDirectory(string) error { return nil }
