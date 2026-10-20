//go:build !windows

package tui

import (
	"fmt"
	"os"
)

// openTerminal opens the terminal to draw on and read keys from: the
// process's controlling terminal, which works even when standard input or
// output is redirected, such as through make or a pipe.
func openTerminal() (*os.File, *os.File, func(), error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("no terminal to show the viewer on (%w); run it in a terminal window", err)
	}
	return f, f, func() { _ = f.Close() }, nil
}
