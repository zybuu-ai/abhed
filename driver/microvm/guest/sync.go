package guest

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"time"
)

// Sync carries the workspace's changes from the guest to the host. After
// the hello the host sends {"type":"sync"}; the guest answers with a delete
// frame for each path gone since the last sync, then a tar archive of every
// file, folder and symlink created or changed since, in tar frames, then
// eof. The host applies them and sends ack; only then does the guest count
// them as synced and answer ok. Without the ack they are sent again next time.
const (
	tSync   = "sync"
	tDelete = "delete"
	tTar    = "tar"
	tEOF    = "eof"
	tAck    = "ack"
)

// slack is how far back a change is looked for before the last sync began:
// an inode's change time comes from a clock up to one kernel tick behind.
const slack = 50 * time.Millisecond

// Change is one path the guest reports changed, for the host to apply.
type Change struct {
	// Deleted is set for a path that is gone; Header is nil then.
	Deleted bool
	Path    string
	Header  *tar.Header
	Body    io.Reader
}

// SyncWith asks the guest over rw for its changes, passes each to apply,
// and acknowledges them when apply has taken them all. An error from apply
// stops the sync unacknowledged, so the guest sends them again.
func SyncWith(ctx context.Context, rw io.ReadWriteCloser, apply func(Change) error) error {
	defer func() { _ = rw.Close() }()
	stop := watch(ctx, rw)
	defer stop()
	c, err := open(rw)
	if err != nil {
		return ctxErr(ctx, err)
	}
	if err := c.write(frame{Type: tSync}); err != nil {
		return ctxErr(ctx, err)
	}
	pr, pw := io.Pipe()
	applied := make(chan error, 1)
	started := false
	startTar := func() {
		started = true
		go func() {
			tr := tar.NewReader(pr)
			var err error
			for {
				var h *tar.Header
				h, err = tr.Next()
				if errors.Is(err, io.EOF) {
					err = nil
					break
				}
				if err != nil {
					break
				}
				if err = apply(Change{Path: path.Clean(h.Name), Header: h, Body: tr}); err != nil {
					break
				}
			}
			// Whatever is left is read, so the frame loop never blocks on it.
			_, _ = io.Copy(io.Discard, pr)
			applied <- err
		}()
	}
	fail := func(err error) error {
		_ = pw.CloseWithError(err)
		if started {
			<-applied
		}
		return ctxErr(ctx, err)
	}
	for {
		f, err := c.read()
		if err != nil {
			return fail(err)
		}
		switch f.Type {
		case tDelete:
			if started {
				return fail(fmt.Errorf("%w: delete after the archive began", errProtocol))
			}
			if err := apply(Change{Deleted: true, Path: f.Path}); err != nil {
				return fail(err)
			}
		case tTar:
			if !started {
				startTar()
			}
			if _, err := pw.Write(f.Data); err != nil {
				return fail(err)
			}
		case tEOF:
			if !started {
				startTar()
			}
			_ = pw.Close()
			if err := <-applied; err != nil {
				return ctxErr(ctx, err)
			}
			if err := c.write(frame{Type: tAck}); err != nil {
				return ctxErr(ctx, err)
			}
			f, err := c.read()
			if err != nil {
				return ctxErr(ctx, err)
			}
			if f.Type != tOK {
				return fmt.Errorf("%w: expected ok, got %q", errProtocol, f.Type)
			}
			return nil
		case tError:
			return fail(&Error{Message: f.Message})
		default:
			return fail(fmt.Errorf("%w: unexpected %q frame", errProtocol, f.Type))
		}
	}
}
