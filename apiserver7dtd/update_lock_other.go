//go:build !linux && !windows

package main

import (
	"errors"
	"os"
)

func lockUpdateState(string) (*os.File, error) {
	return nil, errors.New("update state locking unsupported on this platform")
}
func syncUpdateDirectory(string) error { return errors.New("unsupported platform") }
