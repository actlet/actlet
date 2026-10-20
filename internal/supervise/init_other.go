//go:build !linux

package supervise

// ShouldInit is false here: containers run Linux.
func ShouldInit() bool { return false }

// Init isn't used here.
func Init() int { return 1 }
