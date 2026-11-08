package jobstore

import "os"

// OpenRegularFile opens a job-owned state or stream file for reading. A damaged
// entry such as a FIFO must not block recovery, and a symlink is not a receipt.
func OpenRegularFile(path string) (*os.File, os.FileInfo, error) {
	f, err := openJobRead(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = &os.PathError{Op: "open regular file", Path: path, Err: os.ErrInvalid}
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}
