//go:build !linux && !darwin && !windows

package manager

import (
	"fmt"
	"os"
)

const artifactReadFlags = os.O_RDONLY

func fileIdentity(f *os.File) (string, error) {
	return "", fmt.Errorf("incremental evidence reads require stable file identity on this platform")
}
