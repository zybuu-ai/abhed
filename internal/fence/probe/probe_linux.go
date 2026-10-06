//go:build linux

package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// helperTimeout bounds each helper; every one does a few syscalls.
const helperTimeout = 10 * time.Second

// tcpLandlockABI is the first Landlock ABI that can refuse TCP bind and connect.
const tcpLandlockABI = 4

// cgroupRoot is where the unified hierarchy is mounted.
const cgroupRoot = "/sys/fs/cgroup"

// Run checks this host against req, each time afresh, and never confines the
// calling process: the confining checks run in a re-executed helper.
func Run(ctx context.Context, req Requirements) Report {
	r := Report{OS: runtime.GOOS, Arch: runtime.GOARCH, Checked: time.Now().UTC()}
	var u unix.Utsname
	if unix.Uname(&u) == nil {
		r.Kernel, r.Machine = unix.ByteSliceToString(u.Release[:]), unix.ByteSliceToString(u.Machine[:])
	}
	r.Checks = append(r.Checks, platformCheck(r), notRootCheck())
	r.Checks = append(r.Checks, childChecks(ctx)...)

	abi, abiErr := landlockABI()
	r.LandlockABI = abi
	c := abiCheck(CheckLandlockABI, abi, req.minABI(), "the fence")
	if abiErr != nil {
		c.Reason += ": " + abiErr.Error()
	}
	r.Checks = append(r.Checks, c, landlockFSCheck(ctx, abi), landlockTCPCheck(ctx, abi, req), seccompCheck(ctx), cgroupCheck(req))
	r.finish()
	return r
}

func platformCheck(r Report) Check {
	c := Check{ID: CheckPlatform, Required: true, Value: strings.TrimSpace("Linux " + r.Kernel + " " + r.Machine)}
	if _, ok := seccompArch(); !ok {
		c.Status, c.Reason = Fail, "the fence supports amd64 and arm64, not "+runtime.GOARCH
		return c
	}
	c.Status, c.Reason = Pass, "Linux on "+runtime.GOARCH
	return c
}

func notRootCheck() Check {
	c := Check{ID: CheckNotRoot, Required: true}
	ru, eu, su := unix.Getresuid()
	rg, eg, sg := unix.Getresgid()
	c.Value = fmt.Sprintf("uid %d/%d/%d gid %d/%d/%d", ru, eu, su, rg, eg, sg)
	if ru == 0 || eu == 0 || su == 0 {
		c.Status, c.Reason = Fail, "running as root; the fence refuses root in this release (run Abhed as an ordinary user)"
		return c
	}
	c.Status, c.Reason = Pass, "running as an ordinary user"
	return c
}

// childChecks start the would-be command (a helper that sets no_new_privs and
// re-executes) and judge what it holds.
func childChecks(ctx context.Context) []Check {
	caps := Check{ID: CheckCaps, Required: true}
	nnp := Check{ID: CheckNoNewPrivs, Required: true}
	out, err := runHelper(ctx, stageNNP)
	if err == nil && !out.OK {
		err = errors.New(out.Reason)
	}
	if err != nil {
		caps.Status, caps.Reason = Fail, "could not start a child to inspect: "+err.Error()
		nnp.Status, nnp.Reason = Fail, "could not set no_new_privs in a child: "+err.Error()
		return []Check{caps, nnp}
	}
	st := out.Status

	nnp.Value = "NoNewPrivs " + st["NoNewPrivs"]
	if st["NoNewPrivs"] == "1" {
		nnp.Status, nnp.Reason = Pass, "set in a child and held across its exec"
	} else {
		nnp.Status, nnp.Reason = Fail, "set in a child but not held across its exec"
	}

	caps.Value = fmt.Sprintf("inh %s prm %s eff %s amb %s bnd %s", st["CapInh"], st["CapPrm"], st["CapEff"], st["CapAmb"], st["CapBnd"])
	var held []string
	for _, k := range []string{"CapInh", "CapPrm", "CapEff", "CapAmb"} {
		if v, err := strconv.ParseUint(st[k], 16, 64); err != nil || v != 0 {
			held = append(held, k)
		}
	}
	if len(held) > 0 {
		caps.Status, caps.Reason = Fail, "a child would hold capabilities ("+strings.Join(held, ", ")+"); the fence needs none"
	} else {
		// The bounding set limits what could be gained; with no_new_privs and
		// nothing inheritable or ambient, nothing is.
		caps.Status, caps.Reason = Pass, "a child holds no capabilities (inheritable, permitted, effective and ambient are empty)"
	}
	return []Check{caps, nnp}
}

func landlockFSCheck(ctx context.Context, abi int) Check {
	c := Check{ID: CheckLandlockFS, Required: true}
	if abi == 0 {
		c.Status, c.Reason = Fail, "no Landlock to test"
		return c
	}
	if err := landlockRoundTrip(ctx, &c); err != nil {
		c.Status, c.Reason = Fail, err.Error()
	}
	return c
}

// landlockRoundTrip runs the landlock stage on two fresh folders, then
// confirms on disk that the allowed write landed and the others did not.
func landlockRoundTrip(ctx context.Context, c *Check) error {
	allowed, err := os.MkdirTemp("", "abhed-fence-probe-allow-")
	if err != nil {
		return fmt.Errorf("making a folder to test in: %w", err)
	}
	defer func() { _ = os.RemoveAll(allowed) }()
	denied, err := os.MkdirTemp("", "abhed-fence-probe-deny-")
	if err != nil {
		return fmt.Errorf("making a folder to test in: %w", err)
	}
	defer func() { _ = os.RemoveAll(denied) }()
	existing := filepath.Join(denied, fileExisting)
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		return fmt.Errorf("making a file to test truncation on: %w", err)
	}

	out, err := runHelper(ctx, stageLandlock, allowed, denied)
	if err != nil {
		return err
	}
	if !out.OK {
		return errors.New(out.Reason)
	}
	if b, err := os.ReadFile(filepath.Join(allowed, fileAllowed)); err != nil || string(b) != "ok" { // #nosec G304 -- a file in the probe's own temp folder
		return errors.New("the helper reported a write inside the granted folder, but it is not there")
	}
	if _, err := os.Lstat(filepath.Join(denied, fileDenied)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("a write outside the granted folder landed")
	}
	if b, err := os.ReadFile(existing); err != nil || string(b) != "keep" { // #nosec G304 -- a file in the probe's own temp folder
		return errors.New("a file outside the granted folder was changed")
	}
	c.Status, c.Reason, c.Value = Pass, out.Reason, out.Value
	return nil
}

func landlockTCPCheck(ctx context.Context, abi int, req Requirements) Check {
	if req.AllowNetwork {
		return Check{ID: CheckLandlockTCP, Status: Skip, Reason: "not needed: commands keep the host's network"}
	}
	c := abiCheck(CheckLandlockTCP, abi, tcpLandlockABI, "refusing TCP with the network off")
	if c.Status != Pass {
		return c
	}
	out, err := runHelper(ctx, stageTCP)
	switch {
	case err != nil:
		c.Status, c.Reason = Fail, err.Error()
	case !out.OK:
		c.Status, c.Reason = Fail, out.Reason
	default:
		c.Status, c.Reason, c.Value = Pass, out.Reason, out.Value
	}
	return c
}

func seccompCheck(ctx context.Context) Check {
	c := Check{ID: CheckSeccomp, Required: true}
	mode, err := unix.PrctlRetInt(unix.PR_GET_SECCOMP, 0, 0, 0, 0)
	if err != nil {
		c.Status, c.Reason = Fail, "the kernel has no seccomp: "+err.Error()
		return c
	}
	c.Value = fmt.Sprintf("mode %d", mode)
	out, err := runHelper(ctx, stageSeccomp)
	switch {
	case err != nil:
		c.Status, c.Reason = Fail, err.Error()
	case !out.OK:
		c.Status, c.Reason = Fail, out.Reason
	default:
		c.Status, c.Reason, c.Value = Pass, out.Reason, out.Value
	}
	return c
}

// cgroupCheck requires the unified hierarchy and a subtree this user may make
// a child cgroup in and move a process into. It creates and removes one empty
// child; managing cgroups is not the probe's job.
func cgroupCheck(req Requirements) Check {
	c := Check{ID: CheckCgroup, Required: !req.CgroupOptional}
	dir, value, err := cgroupWritable(req.CgroupDir)
	c.Value = value
	if err != nil {
		c.Status, c.Reason = Fail, err.Error()
		return c
	}
	c.Status, c.Reason = Pass, "a delegated cgroup v2 subtree is writable at "+dir
	return c
}

func cgroupWritable(want string) (dir, value string, err error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(cgroupRoot, &fs); err != nil {
		return "", "", fmt.Errorf("nothing mounted at %s: %w", cgroupRoot, err)
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return "", "", fmt.Errorf("%s is not cgroup v2 (filesystem type %#x); the fence needs the unified hierarchy", cgroupRoot, fs.Type)
	}
	dir = want
	if dir == "" {
		if dir, err = ownCgroup(); err != nil {
			return "", "", err
		}
	}
	dir = filepath.Clean(dir)
	if dir != cgroupRoot && !strings.HasPrefix(dir, cgroupRoot+"/") {
		return "", "", fmt.Errorf("%s is not under %s", dir, cgroupRoot)
	}
	ctrl, _ := os.ReadFile(filepath.Join(dir, "cgroup.controllers")) // #nosec G304 -- a cgroup interface file
	value = dir + " controllers: " + strings.TrimSpace(string(ctrl))

	leaf := filepath.Join(dir, fmt.Sprintf("abhed-fence-probe-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Mkdir(leaf, 0o755); err != nil { // #nosec G301 -- a cgroup folder; the kernel sets its files' modes
		return dir, value, fmt.Errorf("cgroup %s is not delegated to this user: %w", dir, err)
	}
	defer func() { _ = os.Remove(leaf) }()
	f, err := os.OpenFile(filepath.Join(leaf, "cgroup.procs"), os.O_WRONLY, 0) // #nosec G304 -- the probe's own cgroup
	if err != nil {
		return dir, value, fmt.Errorf("cgroup %s lets this user make a child but not move a process into it: %w", dir, err)
	}
	_ = f.Close()
	return dir, value, nil
}

// ownCgroup is this process's cgroup v2 folder, from /proc/self/cgroup.
func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("reading /proc/self/cgroup: %w", err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join(cgroupRoot, p), nil
		}
	}
	return "", errors.New("this process is in no cgroup v2 (no 0:: line in /proc/self/cgroup)")
}

// runHelper runs one helper stage in a re-executed copy of this binary, with
// an empty environment, and returns its answer.
func runHelper(ctx context.Context, stage string, args ...string) (helperOut, error) {
	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/proc/self/exe", append([]string{HelperArg, stage}, args...)...) // #nosec G204 -- this binary, re-executed as the probe's helper
	cmd.Env = []string{}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var out helperOut
	if jerr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); jerr != nil || err != nil {
		why := firstLine(stderr.String())
		if err == nil {
			err = jerr
		}
		if why != "" {
			return out, fmt.Errorf("the probe helper did not answer (%w): %s", err, why)
		}
		return out, fmt.Errorf("the probe helper did not answer (%w)", err)
	}
	return out, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
