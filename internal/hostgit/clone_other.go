//go:build !unix

package hostgit

import "errors"

// moveInto is refused where folders cannot be opened without following links.
func moveInto(string, []string, string) error {
	return errors.New("a clone through host git is not made on this system; clone it yourself")
}
