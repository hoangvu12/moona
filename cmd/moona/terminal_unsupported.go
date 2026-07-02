//go:build !windows

package main

import "errors"

func prepareShortcutConfirmationInput() (func(), error) {
	return func() {}, nil
}

func prepareLocalTerminal() (func(), int, int, error) {
	return func() {}, 0, 0, errors.New("local attach raw mode is only implemented for Windows")
}
