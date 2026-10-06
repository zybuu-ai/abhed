//go:build !darwin && !linux

package app

import "errors"

// writeSysLog is not written yet on Windows or elsewhere: the attempt is in
// admin.jsonl only, and the command says so.
func writeSysLog(string) error {
	return errors.New("not written on this operating system; the attempt is in admin.jsonl only")
}
