//go:build windows

package sandbox

import "os"

// openNoBlock opens a file for reading; Windows has no FIFOs to wait on.
func openNoBlock(path string) (*os.File, error) {
	return os.Open(path)
}

// linkCount is not checked on Windows.
func linkCount(os.FileInfo) uint64 { return 1 }

// openTarget is not available on Windows.
func openTarget(*os.File) (string, bool, error) { return "", false, nil }
