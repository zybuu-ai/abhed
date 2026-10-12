package microvm

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/archive"
	"github.com/zybuu-ai/abhed/driver/microvm/guest"
	"github.com/zybuu-ai/abhed/driver/snapshot"
)

// guestWorkspace is where the guest mounts the workspace image.
const guestWorkspace = "/workspace"

// workspaceRel is p, relative to the workspace or absolute in it (the host's
// path or the guest's), as a clean relative path. One outside it or in
// Abhed's state is refused.
func workspaceRel(in *instance, p string) (string, error) {
	if in.ws == "" {
		return "", driver.Errorf(driver.ErrUnsupported, "microvm: the sandbox has no workspace")
	}
	asked := p
	if filepath.IsAbs(p) {
		p = filepath.Clean(p)
		rel := ""
		for _, base := range []string{in.spec.Workspace, in.ws, guestWorkspace} {
			if base == "" {
				continue
			}
			if r, err := filepath.Rel(base, p); err == nil && filepath.IsLocal(r) || r == "." {
				rel = r
				break
			}
		}
		if rel == "" {
			return "", driver.Errorf(driver.ErrInvalid, "%s is outside the workspace", asked)
		}
		p = rel
	}
	p = filepath.Clean(p)
	if p != "." && !filepath.IsLocal(p) {
		return "", driver.Errorf(driver.ErrInvalid, "%s is outside the workspace", asked)
	}
	if archive.InState(filepath.ToSlash(p)) {
		return "", driver.Errorf(driver.ErrInvalid, "%s is Abhed's state", asked)
	}
	return p, nil
}

// CopyIn unpacks a tar stream into dst, a folder in the workspace, inside
// the guest, which checks every entry and gives it the commands' user; the
// copy then comes back to the host workspace as a command's changes do.
func (d *Driver) CopyIn(ctx context.Context, id, dst string, tar io.Reader) error {
	in, m, cctx, done, err := d.begin(ctx, id)
	if err != nil {
		return err
	}
	defer done()
	rel, err := workspaceRel(in, dst)
	if err != nil {
		return err
	}
	c, err := m.dial(cctx)
	if err != nil {
		return driver.Errorf(driver.ErrUnavailable, "microvm: reaching the guest: %v", err)
	}
	if err := guest.Unpack(cctx, c, filepath.ToSlash(rel), tar); err != nil {
		if ge := (*guest.Error)(nil); errors.As(err, &ge) {
			return driver.Errorf(driver.ErrInvalid, "microvm: copying in: %s", ge.Message)
		}
		return driver.Errorf(driver.ErrUnavailable, "microvm: copying in: %v", err)
	}
	if err := d.syncBack(cctx, in, m, nil); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "microvm: the copy is in the VM, but could not be copied back to the workspace: %v", err)
	}
	return nil
}

// CopyOut writes src, in the workspace, as a tar stream under its own name,
// once the guest's changes are copied back; the host's copy is the one read.
func (d *Driver) CopyOut(ctx context.Context, id, src string, tar io.Writer) error {
	in, m, cctx, done, err := d.begin(ctx, id)
	if err != nil {
		return err
	}
	defer done()
	rel, err := workspaceRel(in, src)
	if err != nil {
		return err
	}
	if err := d.syncBack(cctx, in, m, nil); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "microvm: copying the VM's changes back first: %v", err)
	}
	full := filepath.Join(in.ws, rel)
	if _, err := os.Lstat(full); errors.Is(err, fs.ErrNotExist) {
		return driver.Errorf(driver.ErrNotFound, "%s does not exist in the sandbox's workspace", src)
	} else if err != nil {
		return driver.Errorf(driver.ErrInvalid, "%s cannot be read in the sandbox's workspace", src)
	}
	parent, base := filepath.Dir(full), filepath.Base(full)
	_, err = archive.Pack(cctx, parent, tar, archive.Limits{}, func(name string) bool {
		return name != base && !strings.HasPrefix(name, base+"/")
	})
	return err
}

// forwardStream is one forwarded connection; closing it, or the instance
// stopping, ends it.
type forwardStream struct {
	net.Conn
	once sync.Once
	end  func()
}

func (f *forwardStream) Close() error {
	err := f.Conn.Close()
	f.once.Do(f.end)
	return err
}

// Forward connects to port on the guest's own loopback through the guest
// agent, so it reaches only what the guest could, with no network device.
// ctx bounds only the connecting.
func (d *Driver) Forward(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	if port < 1 || port > 65535 {
		return nil, driver.Errorf(driver.ErrInvalid, "port %d is out of range", port)
	}
	// The stream outlives ctx; it lives until closed or the instance stops.
	in, m, cctx, done, err := d.begin(context.Background(), id)
	if err != nil {
		return nil, err
	}
	c, err := m.dial(ctx)
	if err == nil {
		err = guest.Dial(ctx, c, uint16(port)) // #nosec G115 -- checked above
		if err != nil {
			_ = c.Close()
		}
	}
	if err != nil {
		done()
		if ge := (*guest.Error)(nil); errors.As(err, &ge) {
			return nil, driver.Errorf(driver.ErrUnavailable, "microvm: nothing answered on port %d in the VM: %s", port, ge.Message)
		}
		return nil, driver.Errorf(driver.ErrUnavailable, "microvm: forwarding port %d: %v", port, err)
	}
	base := map[string]any{"sandbox_id": id, "generation": in.spec.Generation, "port": port}
	d.record(driver.EventForward, with(base, "action", "open"))
	st := &forwardStream{Conn: c}
	st.end = func() {
		done()
		d.record(driver.EventForward, with(base, "action", "close"))
	}
	// A stop ends the stream, and with it the stream's hold on the instance.
	context.AfterFunc(cctx, func() { _ = st.Close() })
	return st, nil
}

// Snapshot archives the workspace, without Abhed's state, once a running
// instance's changes are copied back, and records it.
func (d *Driver) Snapshot(ctx context.Context, id, name string) (driver.SnapshotRef, error) {
	if d.opts.SnapshotDir == "" {
		return driver.SnapshotRef{}, driver.Errorf(driver.ErrUnsupported, "microvm: no snapshot_dir is set")
	}
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return driver.SnapshotRef{}, err
	}
	gen, ws := in.spec.Generation, in.ws
	d.mu.Unlock()
	if ws == "" {
		return driver.SnapshotRef{}, driver.Errorf(driver.ErrUnsupported, "microvm: the sandbox has no workspace")
	}
	// As a call on the running VM, so a stop waits for the copy and ends it;
	// a VM not running has nothing to copy back.
	if in, m, cctx, done, err := d.begin(ctx, id); err == nil {
		err = d.syncBack(cctx, in, m, nil)
		done()
		if err != nil {
			return driver.SnapshotRef{}, driver.Errorf(driver.ErrUnavailable, "microvm: copying the VM's changes back first: %v", err)
		}
	}
	ref, st, err := d.snaps.Take(ctx, id, gen, name, ws)
	if err != nil {
		return driver.SnapshotRef{}, err
	}
	if err := d.recordErr(driver.EventSnapshot, snapshot.Payload(id, gen, ref, st)); err != nil {
		_ = d.snaps.Remove(id, ref)
		return driver.SnapshotRef{}, driver.Errorf(driver.ErrUnavailable, "recording the snapshot: %v", err)
	}
	return ref, nil
}

// Restore replaces a stopped instance's workspace with the snapshot, once
// its digest matches, and builds the workspace image again from it, so the
// next start boots with it.
func (d *Driver) Restore(ctx context.Context, id string, ref driver.SnapshotRef) error {
	if d.opts.SnapshotDir == "" {
		return driver.Errorf(driver.ErrUnsupported, "microvm: no snapshot_dir is set")
	}
	d.mu.Lock()
	in, err := d.get(id)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	if in.m != nil || in.starting {
		d.mu.Unlock()
		return driver.Errorf(driver.ErrConflict, "instance %s must be stopped to restore it", id)
	}
	// Held off a start while the workspace and its image are replaced.
	in.starting = true
	ws, gen := in.ws, in.spec.Generation
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		in.starting = false
		d.mu.Unlock()
	}()
	if ws == "" {
		return driver.Errorf(driver.ErrUnsupported, "microvm: the sandbox has no workspace")
	}
	st, err := d.snaps.Restore(ctx, id, ref, ws)
	payload := snapshot.Payload(id, gen, ref, st)
	if err == nil {
		err = d.prepare(ctx, in)
	}
	if err != nil {
		payload["outcome"], payload["error"] = "refused", err.Error()
		d.record(driver.EventRestore, payload)
		return err
	}
	payload["outcome"] = "restored"
	if err := d.recordErr(driver.EventRestore, payload); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "the workspace was restored but the record refused it: %v", err)
	}
	return nil
}

// record appends an event, or warns when the driver has no record.
func (d *Driver) record(event string, payload map[string]any) { _ = d.recordErr(event, payload) }

func (d *Driver) recordErr(event string, payload map[string]any) error {
	if d.opts.Record == nil {
		slog.Debug("microvm: event not recorded: the driver has no record", "event", event, "sandbox_id", payload["sandbox_id"])
		return nil
	}
	return d.opts.Record(event, payload)
}

// with is base and the pairs kv added.
func with(base map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}
