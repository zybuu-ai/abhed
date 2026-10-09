package controlplane

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/zybuu-ai/abhed/driver"
)

// The verbs that reach a driver's optional capabilities: snapshot, restore,
// copy and the driver itself for an exposure. Each checks the caller, the
// capability and this replica's ownership first, so a driver without it is
// refused with ErrUnsupported before anything is recorded or asked of it;
// then the guard and the interceptor admit it (admitVerb).

// capable returns sb's driver, or ErrUnsupported when it lacks c.
func (s *Service) capable(sb Sandbox, c driver.Capability) (driver.Driver, error) {
	d, err := s.cfg.Drivers.Get(sb.Driver)
	if err != nil {
		return nil, fmt.Errorf("%w: driver %s is not registered", ErrUnavailable, sb.Driver)
	}
	if c != "" && !d.Info().Has(c) {
		return nil, driver.Errorf(driver.ErrUnsupported, "driver %s does not support %s", sb.Driver, c)
	}
	return d, nil
}

// prepare loads the sandbox for a verb and checks If-Match, its state and
// the driver's capability.
func (s *Service) prepare(ctx context.Context, c Caller, id, ifMatch, verb string, ok func(State) bool,
	cap driver.Capability) (Sandbox, driver.Driver, error) {
	sb, err := s.Get(ctx, c, id)
	if err != nil {
		return Sandbox{}, nil, err
	}
	if err := CheckIfMatch(ifMatch, sb.ResourceVersion); err != nil {
		return sb, nil, err
	}
	if !ok(sb.State) {
		return sb, nil, fmt.Errorf("%w: sandbox %s is %s; :%s cannot run now", ErrInvalidTransition, id, sb.State, verb)
	}
	d, err := s.capable(sb, cap)
	if err != nil {
		return sb, nil, err
	}
	return sb, d, s.owned(ctx, id)
}

// admitVerb asks the guard and then the interceptor about a verb as the
// operation it amounts to: a restore is a start, and a snapshot, a copy or
// an exposure acts in the sandbox as an exec does, with command naming it.
// So a guard or interceptor that refuses an exec is not gone round by
// another verb. On error the verb's outcome is recorded; release runs once
// the verb has ended.
func (s *Service) admitVerb(ctx context.Context, ev AuditEvent, c Caller, sb Sandbox, op, command string) (
	release func(), adm AdmissionRequest, err error) {
	old := sb.Clone()
	adm = AdmissionRequest{Operation: op, Caller: c, SandboxID: sb.ID, Old: &old, Object: sb}
	ga := &Admission{Action: op, Caller: c, Sandbox: sb}
	if op == OpExec {
		ga.Exec = &ExecRequest{Command: command}
		adm.Command = s.redact(command)
	}
	release, err = s.guardAdmit(ctx, ga)
	if err == nil {
		var next Sandbox
		if next, err = s.admit(ctx, adm); err != nil {
			release()
		} else {
			adm.Object = next
		}
	}
	if err != nil {
		s.outcome(ctx, ev, err)
		return nil, adm, err
	}
	return release, adm, nil
}

// settled reports a state whose instance is not changing.
func settled(st State) bool { return st == StateReady || CanRestore(st) }

// Snapshot asks the sandbox's driver for a snapshot. Ready sandboxes are
// snapshotted while commands may write; a stopped one gives a consistent one.
func (s *Service) Snapshot(ctx context.Context, c Caller, id, ifMatch, name string) (driver.SnapshotRef, Sandbox, error) {
	sb, d, err := s.prepare(ctx, c, id, ifMatch, "snapshot", settled, driver.CapSnapshot)
	if err != nil {
		return driver.SnapshotRef{}, sb, err
	}
	ev := AuditEvent{Principal: c.Principal, Action: "snapshot", SandboxID: id, Generation: sb.Generation,
		Detail: map[string]string{"name": name}}
	if err := s.record(ctx, requested(ev)); err != nil {
		return driver.SnapshotRef{}, sb, err
	}
	release, adm, err := s.admitVerb(ctx, ev, c, sb, OpExec, ":snapshot "+name)
	if err != nil {
		return driver.SnapshotRef{}, sb, err
	}
	defer release()
	ref, err := driver.Snapshot(ctx, d, id, name)
	if err == nil {
		ev.Detail["snapshot_id"], ev.Detail["digest"] = ref.ID, ref.Digest
	}
	s.outcome(ctx, ev, err)
	s.committed(ctx, adm, sb, err)
	if err != nil {
		return driver.SnapshotRef{}, sb, err
	}
	return ref, sb, nil
}

// Restore puts a snapshot back into a stopped, completed or failed sandbox
// and starts it as a new generation. The driver checks the ref is this
// sandbox's and that its content matches the digest.
func (s *Service) Restore(ctx context.Context, c Caller, id, ifMatch string, ref driver.SnapshotRef) (Sandbox, error) {
	sb, d, err := s.prepare(ctx, c, id, ifMatch, "restore", CanRestore, driver.CapSnapshot)
	if err != nil {
		return Sandbox{}, err
	}
	if ref.ID == "" || ref.Digest == "" {
		return Sandbox{}, fmt.Errorf("%w: snapshot must carry its id and digest", ErrInvalid)
	}
	ev := AuditEvent{Principal: c.Principal, Action: "restore", SandboxID: id, Generation: sb.Generation,
		Detail: map[string]string{"snapshot_id": ref.ID, "digest": ref.Digest}}
	if err := s.record(ctx, requested(ev)); err != nil {
		return Sandbox{}, err
	}
	release, adm, err := s.admitVerb(ctx, ev, c, sb, OpStart, "")
	if err != nil {
		return Sandbox{}, err
	}
	defer release()
	err = driver.Restore(ctx, d, id, ref)
	var out Sandbox
	if err == nil {
		out, err = Mutate(ctx, s.cfg.Store, id, "", func(cur *Sandbox) error {
			if err := cur.Restore(s.now()); err != nil {
				return err
			}
			cur.Restarts, cur.NextRestart = 0, nil
			// What an interceptor admitted for the start: its labels and settings.
			cur.Labels, cur.Settings = adm.Object.Labels, adm.Object.Settings
			return nil
		})
		if err != nil {
			err = storeErr(err)
		} else {
			ev.Generation = out.Generation
		}
	}
	s.outcome(ctx, ev, err)
	s.committed(ctx, adm, out, err)
	if err != nil {
		return Sandbox{}, err
	}
	s.Kick(id)
	return out, nil
}

// CopyIn unpacks a tar stream into a Ready sandbox at dst, which the
// driver confines to the workspace.
func (s *Service) CopyIn(ctx context.Context, c Caller, id, dst string, tar io.Reader) (Sandbox, error) {
	sb, d, err := s.prepare(ctx, c, id, "", "cp", AcceptsWork, driver.CapCopy)
	if err != nil {
		return sb, err
	}
	ev := AuditEvent{Principal: c.Principal, Action: "cp", SandboxID: id, Generation: sb.Generation,
		Detail: map[string]string{"direction": "in", "path": s.redact(dst)}}
	if err := s.record(ctx, requested(ev)); err != nil {
		return sb, err
	}
	release, adm, err := s.admitVerb(ctx, ev, c, sb, OpExec, ":cp in "+dst)
	if err != nil {
		return sb, err
	}
	defer release()
	err = driver.CopyIn(ctx, d, id, dst, tar)
	s.outcome(ctx, ev, err)
	s.committed(ctx, adm, sb, err)
	return sb, err
}

// CopyOut writes src from a Ready sandbox to w as a tar stream, stopped
// past the sandbox's exec.output_bytes. started runs once every check has
// passed, before the first byte: an error before it sent nothing.
func (s *Service) CopyOut(ctx context.Context, c Caller, id, src string, w io.Writer, started func(Sandbox)) (Sandbox, error) {
	sb, d, err := s.prepare(ctx, c, id, "", "cp", AcceptsWork, driver.CapCopy)
	if err != nil {
		return sb, err
	}
	ev := AuditEvent{Principal: c.Principal, Action: "cp", SandboxID: id, Generation: sb.Generation,
		Detail: map[string]string{"direction": "out", "path": s.redact(src)}}
	if err := s.record(ctx, requested(ev)); err != nil {
		return sb, err
	}
	release, adm, err := s.admitVerb(ctx, ev, c, sb, OpExec, ":cp out "+src)
	if err != nil {
		return sb, err
	}
	defer release()
	limitBytes := s.cfg.Settings.For(sb.Settings).Int(SetExecOutput)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	limit := driver.NewOutputLimit(limitBytes)
	limit.OnExceed = cancel
	fw := &firstWrite{w: w, first: func() {
		if started != nil {
			started(sb)
		}
	}}
	err = driver.CopyOut(ctx, d, id, src, limit.Wrap(fw))
	if err == nil {
		fw.start()
	}
	if limit.Exceeded() {
		err = fmt.Errorf("%w: the copy was larger than %d bytes and was stopped", driver.ErrOutputLimit, limitBytes)
	}
	s.outcome(ctx, ev, err)
	s.committed(ctx, adm, sb, err)
	return sb, err
}

// UseDriver runs fn with a Ready sandbox's driver, which must declare
// capability c, recording the request as action and fn's outcome. It is
// how a caller outside this package, such as the server's exposures, acts
// on a sandbox through the same checks and record.
func (s *Service) UseDriver(ctx context.Context, c Caller, id, action string, cap driver.Capability, detail map[string]string,
	fn func(d driver.Driver, sb Sandbox) error) (Sandbox, error) {
	sb, d, err := s.prepare(ctx, c, id, "", action, AcceptsWork, cap)
	if err != nil {
		return sb, err
	}
	ev := AuditEvent{Principal: c.Principal, Action: action, SandboxID: id, Generation: sb.Generation, Detail: detail}
	if err := s.record(ctx, requested(ev)); err != nil {
		return sb, err
	}
	command := ":" + action
	for _, k := range slices.Sorted(maps.Keys(detail)) {
		command += " " + k + "=" + detail[k]
	}
	release, adm, err := s.admitVerb(ctx, ev, c, sb, OpExec, command)
	if err != nil {
		return sb, err
	}
	defer release()
	err = fn(d, sb)
	s.outcome(ctx, ev, err)
	s.committed(ctx, adm, sb, err)
	return sb, err
}

// Current reports a sandbox's generation and whether it is Ready, for
// whatever is bound to one incarnation of it. It does not check a caller.
func (s *Service) Current(ctx context.Context, id string) (int64, bool, error) {
	sb, err := s.cfg.Store.Get(ctx, id)
	if err != nil {
		return 0, false, storeErr(err)
	}
	return sb.Generation, AcceptsWork(sb.State), nil
}

// DriverRecorder writes a driver's events (snapshot, restore, forward,
// exposure, egress decisions) to the control plane's record, as the
// driver's principal. Each payload's values are kept as text.
func DriverRecorder(audit AuditLog, now func() time.Time) driver.Recorder {
	if now == nil {
		now = time.Now
	}
	return func(event string, payload map[string]any) error {
		ev := AuditEvent{At: now().UTC(), Principal: DriverPrincipal, Action: event, Outcome: OutcomeOK,
			Detail: map[string]string{}}
		for k, v := range payload {
			switch k {
			case "sandbox_id":
				ev.SandboxID, _ = v.(string)
			case "generation":
				ev.Generation = toInt64(v)
			default:
				ev.Detail[k] = fmt.Sprint(v)
			}
		}
		return audit.Append(context.Background(), ev)
	}
}

// DriverPrincipal is who a driver's own events are recorded as.
const DriverPrincipal = "system:driver"

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

// firstWrite runs first once, before the first byte is written.
type firstWrite struct {
	w     io.Writer
	first func()
	done  bool
}

func (f *firstWrite) start() {
	if !f.done {
		f.done = true
		f.first()
	}
}

func (f *firstWrite) Write(p []byte) (int, error) {
	f.start()
	return f.w.Write(p)
}
