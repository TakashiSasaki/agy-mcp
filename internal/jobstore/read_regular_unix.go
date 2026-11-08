//go:build unix

package jobstore

import (
	"os"
	"syscall"
)

func openJobRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
