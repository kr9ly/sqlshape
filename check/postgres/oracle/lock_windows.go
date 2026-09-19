//go:build windows

package oracle

import "os"

// lockFile is a no-op on Windows: concurrent first fills of the cache are not serialized.
func lockFile(f *os.File) error { return nil }
