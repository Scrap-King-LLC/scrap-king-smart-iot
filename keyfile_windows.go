//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// keyFileSDDL gives SYSTEM and Administrators full control and the service's
// account (LocalService) read access, and nothing to anyone else. "P" stops the
// folder's permissions (Users can read Program Files) from being inherited.
const keyFileSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;LS)"

// writeKeyFile creates path with keyFileSDDL already applied, so the key is
// never readable by anyone else, not even for a moment.
func writeKeyFile(path string, data []byte, replace bool) error {
	if replace {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	sd, err := windows.SecurityDescriptorFromString(keyFileSDDL)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return errKeyExists
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("%w (run this from an Administrator command prompt)", err)
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), path)
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}
