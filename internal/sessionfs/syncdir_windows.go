package sessionfs

// syncDir is a no-op on Windows, which can't sync a directory handle.
func syncDir(string) error { return nil }
