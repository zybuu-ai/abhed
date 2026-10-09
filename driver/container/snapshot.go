package container

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/archive"
	"github.com/zybuu-ai/abhed/driver/forward"
	"github.com/zybuu-ai/abhed/driver/snapshot"
)

// endIncarnation closes what is bound to the running container: its done
// signal and its forwards. It is safe to call again.
func (in *instance) endIncarnation() {
	if in.done != nil {
		close(in.done)
		in.done = nil
	}
	if in.forwards != nil {
		in.forwards.Close()
	}
}

func (d *Driver) record(event string, payload map[string]any) error {
	if d.opts.Record == nil {
		slog.Warn("sandbox event not recorded: the driver has no record", "driver", d.name, "event", event, "sandbox_id", payload["sandbox_id"])
		return nil
	}
	return d.opts.Record(event, payload)
}

// Done is closed when the instance's running container stops.
func (d *Driver) Done(id string) (<-chan struct{}, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in, err := d.get(id)
	if err != nil {
		return nil, err
	}
	if in.stopped || in.done == nil {
		return nil, driver.Errorf(driver.ErrConflict, "instance %s is stopped", id)
	}
	return in.done, nil
}

// Forward runs a relay in the container, as the sandbox's user, that
// connects to port on the container's own loopback. The container gets no
// network for it, under --network none or the allowlist alike: the bytes
// travel through the engine's exec channel. The stream lives until it is
// closed or the container stops; ctx bounds only the connecting.
func (d *Driver) Forward(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	if port < 1 || port > 65535 {
		return nil, driver.Errorf(driver.ErrInvalid, "port %d is out of range", port)
	}
	in, done, err := d.begin(ctx, id)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	fw, gen := in.forwards, in.spec.Generation
	d.mu.Unlock()
	if fw == nil {
		done()
		return nil, driver.Errorf(driver.ErrConflict, "instance %s is stopped", id)
	}
	base := map[string]any{"sandbox_id": id, "generation": gen, "port": port}
	if err := d.record(driver.EventForward, with(base, "action", "open")); err != nil {
		done()
		return nil, driver.Errorf(driver.ErrUnavailable, "recording the forward: %v", err)
	}
	fctx, cancel := context.WithCancel(context.Background())
	pf := pidFile(nonce())
	cmd := d.r.command(fctx, d.execArgs(in, false, in.pol.Workspace, pf, forward.RelayLine(port), nil, nil))
	cmd.Cancel = func() error { d.kill(in, pf); return cmd.Process.Kill() }
	cmd.WaitDelay = 2 * time.Second
	var st *forward.Stream
	st, err = forward.Open(ctx, cmd, port, cancel, func(bin, bout int64) {
		cancel()
		if st != nil {
			fw.Remove(st)
		}
		done()
		_ = d.record(driver.EventForward, with(base, "action", "close", "bytes_in", bin, "bytes_out", bout))
	})
	if err != nil {
		return nil, err
	}
	if err := fw.Add(st); err != nil {
		return nil, err
	}
	return st, nil
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

func (d *Driver) store(in *instance) snapshot.Store {
	s := snapshot.Store{Dir: d.opts.SnapshotDir}
	// A server running as root writes what the sandbox's user must own.
	if os.Geteuid() == 0 && in.uid > 0 {
		s.Owner = &archive.Owner{UID: in.uid, GID: in.gid}
	}
	return s
}

var (
	repoName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	hexID    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// snapshotImage is the name a snapshot's committed image is given:
// localhost/<prefix>snapshot:<sandbox>-<snapshot>, from ids only.
func (d *Driver) snapshotImage(sandboxID, snapID string) (string, error) {
	repo := strings.ToLower(strings.TrimRight(d.opts.prefix(), "-_.")) + "-snapshot"
	tag := strings.ToLower(sandboxID) + "-" + snapID
	if !repoName.MatchString(repo) || !containerID.MatchString(tag) {
		return "", driver.Errorf(driver.ErrInvalid, "the name prefix %q cannot name a snapshot image", d.opts.prefix())
	}
	return "localhost/" + repo + ":" + tag, nil
}

// imageID is an engine's image id as sha256:<hex>; docker prints that form,
// podman the bare hex.
func imageID(out string) (string, bool) {
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) == 0 {
		return "", false
	}
	id := strings.TrimPrefix(strings.ToLower(lines[len(lines)-1]), "sha256:")
	if !hexID.MatchString(id) {
		return "", false
	}
	return "sha256:" + id, true
}

// Snapshot commits the container to an image and archives the workspace,
// without Abhed's state; both are named in the ref and the record. A
// snapshot of a running container is taken while commands may write; stop
// it first for a consistent one.
func (d *Driver) Snapshot(ctx context.Context, id, name string) (driver.SnapshotRef, error) {
	if d.opts.SnapshotDir == "" {
		return driver.SnapshotRef{}, driver.Errorf(driver.ErrUnsupported, "the %s driver has no snapshot folder", d.name)
	}
	if !snapshot.ValidName(name) {
		return driver.SnapshotRef{}, driver.Errorf(driver.ErrInvalid, "snapshot name %q: up to 63 of A-Z a-z 0-9 . _ -", name)
	}
	d.mu.Lock()
	in, err := d.get(id)
	d.mu.Unlock()
	if err != nil {
		return driver.SnapshotRef{}, err
	}
	in.life.Lock()
	defer in.life.Unlock()
	gen := in.spec.Generation
	store := d.store(in)
	ref, st, err := store.Take(ctx, id, gen, name, in.pol.Workspace)
	if err != nil {
		return driver.SnapshotRef{}, err
	}
	img, err := d.snapshotImage(id, ref.ID)
	if err == nil {
		ref.Image, err = d.commit(ctx, in, img)
	}
	if err != nil {
		_ = store.Remove(id, ref)
		return driver.SnapshotRef{}, err
	}
	if err := d.record(driver.EventSnapshot, snapshot.Payload(id, gen, ref, st)); err != nil {
		// A snapshot the record does not hold is not kept.
		_ = store.Remove(id, ref)
		d.removeImage(img)
		return driver.SnapshotRef{}, driver.Errorf(driver.ErrUnavailable, "recording the snapshot: %v", err)
	}
	return ref, nil
}

// commit saves the container as image img and returns the image's id.
func (d *Driver) commit(ctx context.Context, in *instance, img string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, err := d.r.output(cctx, []string{"commit", in.name, img}, nil)
	if err != nil {
		return "", driver.Errorf(driver.ErrUnavailable, "committing the container: %v", err)
	}
	if id, ok := imageID(string(out)); ok {
		return id, nil
	}
	got, err := d.inspectImage(ctx, img)
	if err != nil {
		d.removeImage(img)
		return "", err
	}
	return got, nil
}

func (d *Driver) inspectImage(ctx context.Context, img string) (string, error) {
	ictx, cancel := context.WithTimeout(ctx, engineWait)
	defer cancel()
	out, err := d.r.output(ictx, []string{"image", "inspect", "--format", "{{.Id}}", img}, nil)
	if err != nil {
		return "", driver.Errorf(driver.ErrNotFound, "the snapshot's image %s: %v", img, err)
	}
	id, ok := imageID(string(out))
	if !ok {
		return "", driver.Errorf(driver.ErrInternal, "the engine gave image %s the id %q", img, strings.TrimSpace(string(out)))
	}
	return id, nil
}

func (d *Driver) removeImage(img string) {
	ctx, cancel := context.WithTimeout(context.Background(), engineWait)
	defer cancel()
	if _, err := d.r.output(ctx, []string{"rmi", img}, nil); err != nil {
		slog.Warn("a snapshot image was not removed", "image", img, "err", err)
	}
}

// Restore puts a stopped instance back as the snapshot left it: the
// workspace, once its digest matches, and the committed image, once its id
// matches, which the next start runs. Abhed's state is left as it is.
func (d *Driver) Restore(ctx context.Context, id string, ref driver.SnapshotRef) error {
	if d.opts.SnapshotDir == "" {
		return driver.Errorf(driver.ErrUnsupported, "the %s driver has no snapshot folder", d.name)
	}
	d.mu.Lock()
	in, err := d.get(id)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	in.life.Lock()
	defer in.life.Unlock()
	d.mu.Lock()
	stopped := in.stopped
	d.mu.Unlock()
	if !stopped {
		return driver.Errorf(driver.ErrConflict, "instance %s must be stopped to restore it", id)
	}
	gen := in.spec.Generation
	payload := snapshot.Payload(id, gen, ref, archive.Stats{})
	refuse := func(err error) error {
		payload["outcome"], payload["error"] = "refused", err.Error()
		_ = d.record(driver.EventRestore, payload)
		return err
	}
	if !snapshot.ValidID(ref.ID) {
		return refuse(driver.Errorf(driver.ErrInvalid, "snapshot id %q is not valid", ref.ID))
	}
	if ref.Image == "" {
		return refuse(driver.Errorf(driver.ErrInvalid, "snapshot %s names no image, so the %s driver did not take it", ref.ID, d.name))
	}
	img, err := d.snapshotImage(id, ref.ID)
	if err != nil {
		return refuse(err)
	}
	got, err := d.inspectImage(ctx, img)
	if err != nil {
		return refuse(err)
	}
	if got != strings.ToLower(ref.Image) {
		return refuse(driver.Errorf(driver.ErrInvalid, "snapshot %s's image is %s, not %s; it is not restored", ref.ID, got, ref.Image))
	}
	st, err := d.store(in).Restore(ctx, id, ref, in.pol.Workspace)
	payload = snapshot.Payload(id, gen, ref, st)
	if err != nil {
		return refuse(err)
	}
	in.req.image = img
	payload["outcome"] = "restored"
	if err := d.record(driver.EventRestore, payload); err != nil {
		return driver.Errorf(driver.ErrUnavailable, "the snapshot was restored but the record refused it: %v", err)
	}
	return nil
}

// removeSnapshots deletes a deleted sandbox's snapshots: they restore only
// into it.
func (d *Driver) removeSnapshots(ctx context.Context, id string) error {
	if d.opts.SnapshotDir == "" {
		return nil
	}
	if err := (snapshot.Store{Dir: d.opts.SnapshotDir}).RemoveAll(id); err != nil {
		return err
	}
	prefix, err := d.snapshotImage(id, "")
	if err != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, engineWait)
	defer cancel()
	out, err := d.r.output(lctx, []string{"images", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "label=abhed.sandbox=" + id}, nil)
	if err != nil {
		return driver.Errorf(driver.ErrUnavailable, "listing the snapshot images: %v", err)
	}
	repo, _, _ := strings.Cut(prefix, ":")
	for _, img := range strings.Fields(string(out)) {
		r, tag, _ := strings.Cut(img, ":")
		// Only images named as this driver names them, never one a label alone matches.
		if strings.TrimPrefix(r, "localhost/") == strings.TrimPrefix(repo, "localhost/") && strings.HasPrefix(tag, strings.ToLower(id)+"-snap_") {
			d.removeImage("localhost/" + strings.TrimPrefix(r, "localhost/") + ":" + tag)
		}
	}
	return nil
}
