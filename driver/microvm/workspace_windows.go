package microvm

import (
	"context"
	"errors"
)

func buildImage(context.Context, string, string, string, int, *Credential) (string, error) {
	return "", errors.New("the microvm driver does not run on Windows")
}
