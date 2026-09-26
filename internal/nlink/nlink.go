// Package nlink reads how many names (hard links) a file has: a second name
// for a state file is a path the sandbox does not guard.
package nlink

import (
	"fmt"
	"os"
)

// Linked returns how many names the file at path has, with links followed,
// when it is a regular file with more than one; otherwise 0.
func Linked(path string) (uint64, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if n := Of(info); info.Mode().IsRegular() && n > 1 {
		return n, nil
	}
	return 0, nil
}

// Refusal explains a state file with more than one name, and how to fix it.
func Refusal(path string, n uint64) error {
	return fmt.Errorf("the state file %s has %d names (hard links). A command can rewrite it "+
		"through a name the sandbox does not guard, since the sandbox guards paths, not files. "+
		"Find the other names with `find / -xdev -samefile %s`, remove them, or give the file "+
		"a single name with `cp -p %s %s.new && mv %s.new %s`", path, n, path, path, path, path, path)
}
