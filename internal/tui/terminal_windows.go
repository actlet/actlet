package tui

import (
	"errors"
	"os"

	"golang.org/x/term"
)

// openTerminal returns the console to draw on and read keys from.
func openTerminal() (*os.File, *os.File, func(), error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, nil, nil, errors.New("no console to show the viewer on; run it in a terminal window")
	}
	return os.Stdin, os.Stdout, func() {}, nil
}
