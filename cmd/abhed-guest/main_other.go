//go:build !linux

// Command abhed-guest is the agent inside a microvm driver guest. It runs
// only on Linux, as the guest's init.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "abhed-guest runs only inside a Linux microVM guest, as its init")
	os.Exit(2)
}
