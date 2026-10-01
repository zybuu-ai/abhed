//go:build !unix

package customcmd

// noFollow is not available; the identity check after the open holds alone.
const noFollow = 0
