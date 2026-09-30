//go:build windows

package app

// oNoFollow is not a flag on Windows; openExport's Lstat and the check of
// what was opened stand in for it.
const oNoFollow = 0
