//go:build !windows

package main

import "errors"

func openBrowser(string) error {
	return errors.New("automatic browser opening is only supported on Windows")
}
