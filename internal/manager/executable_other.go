//go:build !linux

package manager

// There is no verified live-executable identity comparison on this platform.
func executableState() (state, advice string) {
	return executableStateUnknown, ""
}
