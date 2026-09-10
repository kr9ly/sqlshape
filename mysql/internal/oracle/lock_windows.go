//go:build windows

package oracle

// lock is a no-op on Windows: one process at a time initializes the template there.
func lock(path string) (unlock func(), err error) {
	return func() {}, nil
}
