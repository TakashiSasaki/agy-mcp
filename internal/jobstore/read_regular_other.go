//go:build !unix

package jobstore

import "os"

func openJobRead(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open regular file", Path: path, Err: os.ErrInvalid}
	}
	return os.Open(path)
}
