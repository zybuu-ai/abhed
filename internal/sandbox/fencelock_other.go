//go:build !unix

package sandbox

import "os"

func stateUseLock(string) *os.File { return nil }

func lastUser(*os.File) bool { return false }

func noteMade(*os.File, folderID) {}

func madeNoted(*os.File, folderID) bool { return false }

func removeMark(string) {}
