package container

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Inside the container, the egress relay and the proxy's socket are here.
const (
	egressDir    = "/run/abhed-egress"
	egressRelay  = egressDir + "/relay"
	egressSocket = egressDir + "/proxy.sock"
)

// keepAlive is the container's own process: it waits, and ends on a stop.
const keepAlive = `trap 'exit 0' TERM INT HUP; while :; do sleep 3600 & wait $!; done`

// execWrapper runs $2 and leaves its pid in $1 while it runs, so a cancel
// can end what it started: an exec's processes share no group to signal.
const execWrapper = `echo $$ > "$1"; /bin/sh -c "$2"; r=$?; rm -f "$1"; exit $r`

// killTree stops the process in the pid file $1 and every descendant, then
// kills them; it reads /proc, so it needs nothing but the shell.
const killTree = `kt() { local d k v; kill -STOP "$1" 2>/dev/null
for d in /proc/[0-9]*; do while read -r k v _; do if [ "$k" = PPid: ]; then [ "$v" = "$1" ] && kt "${d#/proc/}"; break; fi; done < "$d/status" 2>/dev/null; done
kill -KILL "$1" 2>/dev/null; }
read -r p < "$1" 2>/dev/null && [ -n "$p" ] && kt "$p"; rm -f "$1"`

// shellLine prefers bash where the image has it.
const shellLine = `if command -v bash >/dev/null 2>&1; then exec bash --noprofile --norc -i; fi; exec sh -i`

// verifyScript prints what the kernel says is in force for a process in the
// container, the network devices it can see with their state and flags, and
// the addresses on them, so an idle tunnel device can be told from a link.
const verifyScript = `while read -r k v _; do case "$k" in Seccomp:|NoNewPrivs:|CapEff:|CapBnd:|Uid:) echo "$k $v";; esac; done < /proc/self/status
while read -r l; do echo "net $l"; done < /proc/net/dev
for d in /sys/class/net/*; do s=; f=; read -r s < "$d/operstate"; read -r f < "$d/flags"; echo "dev ${d##*/} $s $f"; done 2>/dev/null
if [ -r /proc/net/fib_trie ]; then echo "fib-read"; while read -r l; do echo "fib $l"; done < /proc/net/fib_trie; fi
while read -r l; do echo "inet6 $l"; done 2>/dev/null < /proc/net/if_inet6
if ( : > /.abhed-rootfs-probe ) 2>/dev/null; then echo "rootfs writable"; fi`

// runArgs is the engine argv that starts in's container.
func (d *Driver) runArgs(in *instance) []string {
	eng, pol, req := d.eng, in.pol, in.req
	args := []string{"run", "-d", "--name", in.name,
		"--label", "abhed.sandbox=" + in.spec.ID,
		"--label", "abhed.driver=" + d.name,
		"--label", "abhed.generation=" + strconv.FormatInt(in.spec.Generation, 10),
		// A real init reaps what commands leave behind and passes a stop on.
		"--init",
		// No capability, and no way to regain one through a setuid binary.
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
	}
	if d.opts.SeccompProfile != "" {
		args = append(args, "--security-opt", "seccomp="+d.opts.SeccompProfile)
	}
	// The image is immutable; scratch space is sized and cannot run what is
	// written there. Podman would otherwise add an executable /var/tmp.
	args = append(args, "--read-only")
	if eng.podman {
		args = append(args, "--read-only-tmpfs=false")
	}
	args = append(args,
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=256m",
		"--tmpfs", "/run:rw,noexec,nosuid,nodev,size=16m",
	)
	// As the container tier: nproc only without a pids limit, since it
	// counts the host's processes of the same uid under rootful Docker.
	if req.pids <= 0 {
		args = append(args, "--ulimit", "nproc=256:256")
	}
	args = append(args,
		"--ulimit", "nofile=1024:1024",
		"--ulimit", "fsize=536870912:536870912",
		"--ulimit", "core=0:0",
		"--cpus", strconv.FormatFloat(req.cpus, 'f', -1, 64),
		"--ipc", "private", "--hostname", "abhed",
	)
	// Podman takes PID and UTS from containers.conf, which could say host.
	if eng.podman {
		args = append(args, "--pid", "private", "--uts", "private")
	}
	if !pol.AllowNetwork || pol.Egress != nil {
		args = append(args, "--network", "none")
	}
	if req.memoryMB > 0 {
		m := strconv.Itoa(req.memoryMB) + "m"
		args = append(args, "--memory", m, "--memory-swap", m)
	}
	if req.pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(req.pids))
	}
	// Rootless Podman maps this user to root inside unless told to keep it.
	if eng.podman && eng.rootless {
		args = append(args, "--userns", "keep-id")
	}
	args = append(args, "--user", in.user)
	args = append(args, relabel(in.mounts, pol.Workspace, eng.selinux)...)
	for _, m := range req.mounts {
		opt := "ro"
		if m.Writable {
			opt = "rw"
		}
		if eng.selinux && m.Relabel {
			opt += ",z"
		}
		args = append(args, "-v", m.Host+":"+m.Target+":"+opt)
	}
	for _, g := range req.gpus {
		args = append(args, "--device", g)
	}
	switch {
	case in.egress != nil && in.egress.stdio:
		// The operator's Linux build, on the engine's host, is the relay.
		opt := "ro"
		if eng.selinux {
			opt += ",z"
		}
		args = append(args, "-v", d.opts.RelayBinary+":"+egressRelay+":"+opt)
	case in.egress != nil:
		opt := "ro"
		if eng.selinux {
			opt += ",Z"
		}
		args = append(args, "-v", in.egress.dir+":"+egressDir+":"+opt)
	}
	args = append(args, "-w", pol.Workspace,
		"-e", "ABHED_SANDBOX="+string(sandbox.TierContainer), "-e", "HOME=/tmp")
	if in.egress != nil && !in.egress.stdio {
		args = append(args, "--entrypoint", egressRelay, req.image,
			egressRelayArg, egressSocket, in.egress.listen, "--", "/bin/sh", "-c", keepAlive)
		return args
	}
	return append(args, "--entrypoint", "/bin/sh", req.image, "-c", keepAlive)
}

// relabel adds a shared SELinux label to the mounts of the workspace and of
// what lies in it, on a host that enforces labels; never to /dev/null or a
// system folder from read_only_paths, which the engine would refuse.
func relabel(mounts []string, workspace string, selinux bool) []string {
	out := append([]string{}, mounts...)
	if !selinux {
		return out
	}
	ws := sandbox.PathForms(workspace)
	for i := 0; i+1 < len(out); i++ {
		if out[i] != "-v" {
			continue
		}
		parts := strings.Split(out[i+1], ":")
		if len(parts) < 2 || parts[0] == "/dev/null" || !insideAny(parts[0], ws) {
			continue
		}
		if len(parts) == 2 {
			out[i+1] += ":z"
		} else {
			out[i+1] += ",z"
		}
	}
	return out
}

func insideAny(p string, roots []string) bool {
	for _, r := range roots {
		if _, ok := sandbox.Within(p, r); ok {
			return true
		}
	}
	return false
}

// execArgs is the argv running line in the container, as the sandbox user,
// in dir, with the named variables forwarded by name from the CLI's own
// environment, so no value appears in an argument.
func (d *Driver) execArgs(in *instance, tty bool, dir, pidFile, line string, names, fixed []string) []string {
	args := []string{"exec", "-i"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, "--user", in.user, "-w", dir)
	for _, kv := range fixed {
		args = append(args, "-e", kv)
	}
	for _, n := range names {
		args = append(args, "-e", n)
	}
	return append(args, in.name, "/bin/sh", "-c", execWrapper, "abhed-exec", pidFile, line)
}

// pidFile is where an exec's wrapper leaves its pid.
func pidFile(nonce string) string { return filepath.Join("/tmp", ".abhed-exec-"+nonce) }

// userFor is the uid:gid commands run as: this user, or the workspace's
// owner when Abhed runs as root, or nobody; never root, except under
// rootless Docker, where root inside is this user outside.
func userFor(uid, gid, wsUID, wsGID int, eng engineInfo) (int, int) {
	switch {
	case eng.rootless && !eng.podman:
		return 0, 0
	case uid > 0:
		return uid, gid
	case wsUID > 0:
		return wsUID, wsGID
	}
	return 65534, 65534
}
