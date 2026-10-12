//go:build !windows

package guest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// placeCA writes the bundle the host streams to s.CAFile, whole or not at all,
// readable by commands and writable only by the agent.
func (s *Server) placeCA(c *conn) error {
	b, err := readCA(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.CAFile), ".abhed-ca-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil { // #nosec G302 -- public certificates, read by commands
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.CAFile)
}

// readCA reads the bundle a host streams, within MaxCABundle.
func readCA(c *conn) ([]byte, error) {
	var b bytes.Buffer
	for {
		f, err := c.read()
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case tStdin:
			if b.Len()+len(f.Data) > MaxCABundle {
				return nil, fmt.Errorf("the CA bundle is over %d bytes", MaxCABundle)
			}
			b.Write(f.Data)
		case tStdinEOF:
			if !bytes.Contains(b.Bytes(), []byte("-----BEGIN CERTIFICATE-----")) {
				return nil, errors.New("the CA bundle holds no certificate")
			}
			return b.Bytes(), nil
		default:
			return nil, fmt.Errorf("%w: unexpected %q frame in a CA bundle", errProtocol, f.Type)
		}
	}
}
