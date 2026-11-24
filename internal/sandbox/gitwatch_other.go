//go:build !linux

package sandbox

// gitWatch is Linux's: elsewhere bubblewrap does not run.
type gitWatch struct{}

func (s *Process) watchGit([]string) {}

func (s *Process) takePlanted() []map[string]any { return nil }

func (s *Process) stopGitWatch() {}
