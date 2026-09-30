//go:build windows

package local

// oNoFollow is not a flag on Windows; openOwn's check of what it opened
// stands in for it.
const oNoFollow = 0
