//go:build !linux

package cgroup

import "context"

func askSystemd(context.Context, string, bool) (string, error) { return "", ErrUnsupported }

func ownedByUser(string) error { return ErrUnsupported }
