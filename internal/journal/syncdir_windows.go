package journal

// syncDir is a no-op on Windows, which has no way to sync a directory
// handle; NTFS journals its metadata itself.
func syncDir(string) error { return nil }
