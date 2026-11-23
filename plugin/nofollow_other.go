//go:build !unix

package plugin

// oNoFollow is not available; the walk's own check of each entry stands.
const oNoFollow = 0
