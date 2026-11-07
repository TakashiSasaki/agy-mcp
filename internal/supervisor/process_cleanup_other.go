//go:build !linux

package supervisor

// Non-Linux platforms retain existing process-group or Windows Job Object
// semantics; this does not claim new-session descendant adoption on macOS.
func newProcessCleanup() (processCleanup, error) { return nil, nil }
