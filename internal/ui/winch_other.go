//go:build !unix

package ui

// resizeSignalled is false where no signal announces a resize: the dock's
// ticker notices a new size instead.
const resizeSignalled = false

func watchResize(<-chan struct{}, func()) {}
