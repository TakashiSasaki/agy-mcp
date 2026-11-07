//go:build linux

package manager

import (
	"errors"
	"os"
)

// executableState compares this process's executable inode with its current
// filesystem path. It does not hash files, run binaries or inspect other
// processes. Go's os.Executable removes Linux's " (deleted)" suffix.
func executableState() (state, advice string) {
	return inspectExecutableState(os.Executable, os.Stat)
}

func inspectExecutableState(executable func() (string, error), stat func(string) (os.FileInfo, error)) (state, advice string) {
	path, err := executable()
	if err != nil || path == "" {
		return executableStateUnknown, ""
	}
	running, err := stat("/proc/self/exe")
	if err != nil || !running.Mode().IsRegular() {
		return executableStateUnknown, ""
	}
	installed, err := stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", "The running executable's path is missing. Restore the installation before reconnecting this MCP server."
	}
	if err != nil || !installed.Mode().IsRegular() {
		return executableStateUnknown, ""
	}
	if !os.SameFile(running, installed) {
		return "replaced", "The running executable differs from its installed path. Reconnect this MCP server to load the replacement."
	}
	return "current", ""
}
