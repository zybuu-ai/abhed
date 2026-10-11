package container

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Options are the operator's settings for a container driver. A Spec may
// only narrow them: it can pick from what they list and lower what they
// bound, never add to either.
type Options struct {
	// Binary is the engine's CLI; found on PATH by the engine's name when empty.
	Binary string
	// Image is the default image: ABHED_SANDBOX_IMAGE, or the container
	// tier's default, when empty.
	Image string
	// Images are further images a Spec may name in Config["image"].
	Images []string
	// Mounts are extra host folders every sandbox gets, after admission.
	Mounts []Mount
	// MountRoots are where a Spec's own mounts (Config["mounts"]) may come
	// from; with none, a Spec may not ask for mounts.
	MountRoots []MountRoot
	// GPUs are the CDI device names, such as nvidia.com/gpu=all, a Spec may
	// ask for in Config["gpus"]. None by default.
	GPUs []string
	// CPUs bounds each sandbox's CPU; 2 when zero.
	CPUs float64
	// SeccompProfile is a seccomp profile file to use instead of the
	// engine's default; "unconfined" is refused.
	SeccompProfile string
	// NamePrefix starts every container's name; "abhed-sbx-" when empty.
	NamePrefix string
	// Record receives the egress decisions of a sandbox under the network
	// allowlist, and its snapshots, restores and forwards, with the
	// sandbox's id in the payload.
	Record func(event string, payload map[string]any) error
	// SnapshotDir holds workspace snapshots, outside every workspace; the
	// driver declares snapshot only when it is set.
	SnapshotDir string
	// Reserved are host folders no mount root may overlap, such as the one
	// holding every sandbox's workspace: a mount there would hand one
	// sandbox the others' files. SnapshotDir is reserved too.
	Reserved []string
	// RelayBinary is a Linux build of Abhed for the engine's architecture,
	// at a path the engine's host can see. With it, the network allowlist
	// works where the engine runs in a VM or on another host: the relay
	// inside reaches the proxy over a long-lived exec's stdio instead of a
	// host socket.
	RelayBinary string
}

// Mount is an extra host folder seen inside the sandbox. It is read-only
// unless Writable.
type Mount struct {
	Host     string `json:"host"`
	Target   string `json:"target"`
	Writable bool   `json:"rw,omitempty"`
	// Relabel gives the folder a label every container may share, on an
	// SELinux host; the engine refuses it for system folders.
	Relabel bool `json:"relabel,omitempty"`
}

// MountRoot is a host folder under which a Spec may ask for mounts.
type MountRoot struct {
	Path string
	// Writable lets a Spec's mount under it be writable.
	Writable bool
	// Relabel relabels a Spec's mounts under it on an SELinux host.
	Relabel bool
}

const (
	defaultPrefix = "abhed-sbx-"
	defaultCPUs   = 2.0
)

// cdiName is a CDI device name: vendor/class=device.
var cdiName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*\.[a-z]{2,}/[a-z0-9][a-z0-9_.-]*=[A-Za-z0-9][A-Za-z0-9_.:-]*$`)

// containerID is what a sandbox id may hold to name a container.
var containerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func (o Options) image() string {
	if o.Image != "" {
		return o.Image
	}
	return sandbox.Image
}

func (o Options) cpus() float64 {
	if o.CPUs > 0 {
		return o.CPUs
	}
	return defaultCPUs
}

func (o Options) prefix() string {
	if o.NamePrefix != "" {
		return o.NamePrefix
	}
	return defaultPrefix
}

// check refuses options that could never be admitted.
func (o Options) check() error {
	for _, img := range append([]string{o.image()}, o.Images...) {
		if err := checkImage(img); err != nil {
			return err
		}
	}
	for _, g := range o.GPUs {
		if !cdiName.MatchString(g) {
			return driver.Errorf(driver.ErrInvalid, "GPU %q is not a CDI device name such as nvidia.com/gpu=all", g)
		}
	}
	if o.RelayBinary != "" && !filepath.IsAbs(o.RelayBinary) {
		return driver.Errorf(driver.ErrInvalid, "relay_binary %q is not an absolute path", o.RelayBinary)
	}
	if o.SeccompProfile != "" {
		if strings.EqualFold(o.SeccompProfile, "unconfined") || !filepath.IsAbs(o.SeccompProfile) {
			return driver.Errorf(driver.ErrInvalid, "the seccomp profile must be an absolute path to a profile; unconfined is refused")
		}
		if err := safeArg(o.SeccompProfile); err != nil {
			return err
		}
	}
	if !containerID.MatchString(o.prefix()) {
		return driver.Errorf(driver.ErrInvalid, "name prefix %q cannot start a container name", o.NamePrefix)
	}
	for _, r := range o.MountRoots {
		real, err := admitHost(r.Path, nil)
		if err != nil {
			return fmt.Errorf("mount root: %w", err)
		}
		for _, res := range append(slices.Clone(o.Reserved), o.SnapshotDir) {
			if res == "" {
				continue
			}
			for _, form := range sandbox.PathForms(res) {
				if overlaps(real, form) || overlaps(r.Path, form) {
					return driver.Errorf(driver.ErrInvalid, "mount root %s overlaps %s, where sandbox workspaces or snapshots are kept", r.Path, res)
				}
			}
		}
	}
	return nil
}

func checkImage(img string) error {
	if img == "" || strings.HasPrefix(img, "-") || strings.ContainsAny(img, " \t\r\n") {
		return driver.Errorf(driver.ErrInvalid, "image %q is not an image reference", img)
	}
	return nil
}

// safeArg refuses a value that would split a -v or --mount argument.
func safeArg(s string) error {
	if strings.ContainsAny(s, ":,\x00\r\n") {
		return driver.Errorf(driver.ErrInvalid, "path %q holds a character a mount cannot carry (: , or a control character)", s)
	}
	return nil
}

// request is what a Spec's Config asked for, after admission.
type request struct {
	image    string
	mounts   []Mount // admitted: Host is the real path
	gpus     []string
	memoryMB int
	pids     int
	cpus     float64
}

// configKeys are the Spec.Config keys this driver reads; any other is refused.
var configKeys = []string{"image", "mounts", "gpus", "memory_mb", "pids", "cpus"}

// admit reads spec.Config against the options and the policy's bounds.
// Anything it cannot honour exactly is refused, never dropped.
func (o Options) admit(spec driver.Spec, pol sandbox.Policy, deny []string) (request, error) {
	for k := range spec.Config {
		if !slices.Contains(configKeys, k) {
			return request{}, driver.Errorf(driver.ErrInvalid, "config %q is not a setting of this driver (it reads %s)", k, strings.Join(configKeys, ", "))
		}
	}
	req := request{image: o.image(), memoryMB: pol.MaxMemoryMB, pids: pol.MaxProcs, cpus: o.cpus()}
	if img := spec.Config["image"]; img != "" && img != req.image {
		if !slices.Contains(o.Images, img) {
			return request{}, driver.Errorf(driver.ErrInvalid, "image %q is not one this driver is configured to run", img)
		}
		req.image = img
	}
	if v := spec.Config["memory_mb"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || (req.memoryMB > 0 && n > req.memoryMB) {
			return request{}, driver.Errorf(driver.ErrInvalid, "memory_mb %q must be a positive number no higher than %d", v, req.memoryMB)
		}
		req.memoryMB = n
	}
	if v := spec.Config["pids"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || (req.pids > 0 && n > req.pids) {
			return request{}, driver.Errorf(driver.ErrInvalid, "pids %q must be a positive number no higher than %d", v, req.pids)
		}
		req.pids = n
	}
	if v := spec.Config["cpus"]; v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > req.cpus {
			return request{}, driver.Errorf(driver.ErrInvalid, "cpus %q must be a positive number no higher than %g", v, req.cpus)
		}
		req.cpus = f
	}
	if v := spec.Config["gpus"]; v != "" {
		for _, g := range strings.Split(v, ",") {
			g = strings.TrimSpace(g)
			if !cdiName.MatchString(g) {
				return request{}, driver.Errorf(driver.ErrInvalid, "GPU %q is not a CDI device name; raw device paths are refused", g)
			}
			if !slices.Contains(o.GPUs, g) {
				return request{}, driver.Errorf(driver.ErrInvalid, "GPU %q is not one this driver is configured to pass through", g)
			}
			if !slices.Contains(req.gpus, g) {
				req.gpus = append(req.gpus, g)
			}
		}
	}

	// The operator's mounts first, then the Spec's, each from a root.
	targets := map[string]bool{}
	add := func(m Mount) error {
		admitted, err := admitMount(m, pol.Workspace, deny)
		if err != nil {
			return err
		}
		if targets[admitted.Target] {
			return driver.Errorf(driver.ErrInvalid, "two mounts at %s", admitted.Target)
		}
		targets[admitted.Target] = true
		req.mounts = append(req.mounts, admitted)
		return nil
	}
	for _, m := range o.Mounts {
		if err := add(m); err != nil {
			return request{}, err
		}
	}
	if v := spec.Config["mounts"]; v != "" {
		var asked []Mount
		dec := json.NewDecoder(strings.NewReader(v))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&asked); err != nil {
			return request{}, driver.Errorf(driver.ErrInvalid, "mounts must be a JSON list of {host, target, rw}: %v", err)
		}
		for _, m := range asked {
			root, err := o.rootFor(m.Host, deny)
			if err != nil {
				return request{}, err
			}
			if m.Writable && !root.Writable {
				return request{}, driver.Errorf(driver.ErrInvalid, "mount %s is asked writable, but %s allows read-only mounts only", m.Host, root.Path)
			}
			m.Relabel = root.Relabel
			if err := add(m); err != nil {
				return request{}, err
			}
		}
	}
	return req, nil
}

// rootFor is the configured root holding host, judged with links followed,
// so a link inside a root cannot reach outside it.
func (o Options) rootFor(host string, deny []string) (MountRoot, error) {
	real, err := admitHost(host, deny)
	if err != nil {
		return MountRoot{}, err
	}
	for _, r := range o.MountRoots {
		rr, err := filepath.EvalSymlinks(r.Path)
		if err != nil {
			continue
		}
		if _, ok := sandbox.Within(real, rr); ok {
			return r, nil
		}
	}
	return MountRoot{}, driver.Errorf(driver.ErrInvalid, "mount %s is not under a folder this driver lets a sandbox mount from", host)
}

// admitMount checks one mount and returns it with the host's real path.
func admitMount(m Mount, workspace string, deny []string) (Mount, error) {
	real, err := admitHost(m.Host, deny)
	if err != nil {
		return Mount{}, err
	}
	t := m.Target
	if t == "" {
		t = m.Host
	}
	if !filepath.IsAbs(t) || filepath.Clean(t) != t {
		return Mount{}, driver.Errorf(driver.ErrInvalid, "mount target %q must be an absolute, clean path", t)
	}
	if err := safeArg(t); err != nil {
		return Mount{}, err
	}
	// The workspace, the scratch folders and the egress relay are the
	// driver's; a mount over or under them would change what they hold.
	if t == "/" {
		return Mount{}, driver.Errorf(driver.ErrInvalid, "a mount cannot replace the root folder")
	}
	for _, own := range []string{"/proc", "/sys", "/dev", "/run", "/tmp", workspace} {
		if overlaps(t, own) {
			return Mount{}, driver.Errorf(driver.ErrInvalid, "mount target %s overlaps %s, which the sandbox itself provides", t, own)
		}
	}
	return Mount{Host: real, Target: t, Writable: m.Writable, Relabel: m.Relabel}, nil
}

// overlaps reports whether a is b, inside b, or holds b.
func overlaps(a, b string) bool {
	if _, ok := sandbox.Within(a, b); ok {
		return true
	}
	_, ok := sandbox.Within(b, a)
	return ok
}

// admitHost checks a host path for a mount and returns it with links
// followed. It refuses what would hand the sandbox the host: the kernel's
// filesystems, devices, an engine's socket, Abhed's state, a home folder
// whole, and any Unix socket.
func admitHost(host string, deny []string) (string, error) {
	if host == "" || !filepath.IsAbs(host) || filepath.Clean(host) != host {
		return "", driver.Errorf(driver.ErrInvalid, "mount %q must be an absolute, clean path", host)
	}
	if err := safeArg(host); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(host)
	if err != nil {
		return "", driver.Errorf(driver.ErrInvalid, "mount %s: %v", host, err)
	}
	if err := safeArg(real); err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", driver.Errorf(driver.ErrInvalid, "mount %s: %v", host, err)
	}
	if info.Mode()&os.ModeSocket != 0 {
		return "", driver.Errorf(driver.ErrInvalid, "mount %s is a socket; sockets are never mounted", host)
	}
	if info.Mode()&(os.ModeDevice|os.ModeCharDevice|os.ModeNamedPipe) != 0 {
		return "", driver.Errorf(driver.ErrInvalid, "mount %s is a device or pipe", host)
	}
	for _, d := range append(sensitive(), deny...) {
		for _, form := range sandbox.PathForms(d) {
			if overlaps(real, form) || overlaps(host, form) {
				return "", driver.Errorf(driver.ErrInvalid, "mount %s is refused: it overlaps %s", host, d)
			}
		}
	}
	for _, h := range homes() {
		for _, form := range sandbox.PathForms(h) {
			if _, ok := sandbox.Within(form, real); ok {
				return "", driver.Errorf(driver.ErrInvalid, "mount %s is refused: it holds the home folder %s", host, h)
			}
		}
	}
	return real, nil
}

// sensitive are host paths no mount may be, hold or sit inside.
func sensitive() []string {
	s := []string{"/proc", "/sys", "/dev", "/run", "/var/run", "/etc/abhed",
		"/var/lib/docker", "/var/lib/containers", "/var/lib/containerd"}
	for _, k := range []string{"DOCKER_HOST", "CONTAINER_HOST"} {
		if v, ok := strings.CutPrefix(os.Getenv(k), "unix://"); ok && v != "" {
			s = append(s, v)
		}
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		s = append(s, x)
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, d := range []string{".abhed", ".docker", ".rd", ".colima", ".lima", ".orbstack",
			".local/share/containers", ".config/containers", "Library/Application Support/Abhed Studio"} {
			s = append(s, filepath.Join(home, d))
		}
	}
	return s
}

// homes are the folders a whole home lives in, and this user's own.
func homes() []string {
	h := []string{"/home", "/Users", "/root"}
	if home, err := os.UserHomeDir(); err == nil {
		h = append(h, home)
	}
	return h
}
