package guest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// tCA places the egress proxy's inspection CA bundle, streamed as stdin
// frames, at the guest's CA file, outside the workspace.
const tCA = "ca"

// CAFile is where the guest agent keeps the CA bundle; commands' certificate
// variables name it.
const CAFile = "/run/abhed-ca.pem"

// MaxCABundle bounds the CA bundle a guest takes.
const MaxCABundle = 4 << 20

// PlaceCA sends bundle to the guest, which writes it where its commands'
// clients are pointed. A guest that does not know the call refuses it.
func PlaceCA(ctx context.Context, rw io.ReadWriteCloser, bundle []byte) error {
	defer func() { _ = rw.Close() }()
	if len(bundle) == 0 || len(bundle) > MaxCABundle {
		return fmt.Errorf("a CA bundle of %d bytes", len(bundle))
	}
	stop := watch(ctx, rw)
	defer stop()
	c, err := open(rw)
	if err != nil {
		return ctxErr(ctx, err)
	}
	if err := c.write(frame{Type: tCA}); err != nil {
		return ctxErr(ctx, err)
	}
	// Sent while the answer is read: a guest that refuses at once stops reading.
	go func() {
		if c.chunks(tStdin, bundle) == nil {
			_ = c.write(frame{Type: tStdinEOF})
		}
	}()
	f, err := c.read()
	if err != nil {
		return ctxErr(ctx, err)
	}
	switch f.Type {
	case tOK:
		return nil
	case tError:
		return &Error{Message: f.Message}
	}
	return fmt.Errorf("%w: unexpected %q frame", errProtocol, f.Type)
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
