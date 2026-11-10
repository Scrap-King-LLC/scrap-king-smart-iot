//go:build !windows

package main

import (
	"errors"
	"os"
)

// writeKeyFile creates path readable by its owner only.
func writeKeyFile(path string, data []byte, replace bool) error {
	if replace {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return errKeyExists
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}
