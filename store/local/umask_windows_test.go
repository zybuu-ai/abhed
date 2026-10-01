//go:build windows

package local

func setUmask(m int) int { return m }
