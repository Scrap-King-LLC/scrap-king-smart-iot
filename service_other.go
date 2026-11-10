//go:build !windows

package main

import "errors"

// The service commands exist only on Windows. `run` and `check` work anywhere,
// which is how the agent is developed and tested on Linux or macOS.

var errNotWindows = errors.New("the service commands only work on Windows; use `run` to run in the foreground")

func runningAsService() bool         { return false }
func runAsService(string) error      { return errNotWindows }
func installService(string) error    { return errNotWindows }
func uninstallService() error        { return errNotWindows }
func startService() error            { return errNotWindows }
func stopService() error             { return errNotWindows }
func serviceStatus() (string, error) { return "", errNotWindows }
