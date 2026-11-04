//go:build linux || darwin

package manager

import (
	"fmt"
	"os"
	"syscall"
)

const artifactReadFlags = os.O_RDONLY | syscall.O_NONBLOCK

func fileIdentity(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("file identity unavailable")
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}
