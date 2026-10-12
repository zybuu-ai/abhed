//go:build !darwin && !linux && !windows

package app

import "errors"

// writeSysLog is not written on this system: the attempt is in
// admin.jsonl only, and the command says so.
func writeSysLog(string) error {
	return errors.New("not written on this operating system; the attempt is in admin.jsonl only")
}
