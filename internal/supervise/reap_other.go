//go:build !linux

package supervise

// Supervise does nothing here: only Linux lets a process adopt its
// orphaned descendants.
func Supervise() (stop func()) { return func() {} }

func killDescendants() {}

func descendants() int { return 0 }
