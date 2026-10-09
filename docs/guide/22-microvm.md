# The microVM driver

The `microvm` driver runs each sandbox in a small virtual machine with its own
guest kernel. On Linux the VM is a Firecracker microVM started by
Firecracker's jailer on KVM; on macOS it would be a VM on Apple's
Virtualization.framework, run by a separate signed helper (see "macOS" below).
Instances report the isolation tier `microvm`.

This is hardware virtualization, and it is a different thing from the `vm`
sandbox tier. The `vm` tier is gVisor: a user-space kernel in front of a
container, sharing the host kernel. Neither is described as the other
anywhere in Abhed.

**Status: experimental, not yet used by `abhed serve`.** The driver
implements the driver interface (`docs/architecture/control-plane-contract.md`)
and can be served to a server over the out-of-process driver protocol, but
nothing in Abhed creates microVM sandboxes yet; per-session sandboxes come
with the control plane work. The driver's process handling, files and wire
are tested on every platform against a stand-in VMM; booting a real microVM
is a separate test that needs a KVM host (see "Testing on KVM").

## What is isolated, and what is not

| | |
|---|---|
| Kernel | The guest runs its own Linux kernel. A command that exploits a kernel bug compromises the guest kernel, not the host's. |
| Network | The VM has **no network device**, ever. The only interface in the guest is loopback. With the network off, the host listens on no port for the guest and the guest forwards nothing: there is no path out. With the egress allowlist on, the session's egress proxy is the only way out (see "Network"). |
| Host channel | One vsock connection to the guest agent. The host side is a Unix socket in the VM's chroot; the driver opens it without following a symlink, so a compromised VMM cannot point it elsewhere. |
| Files | The workspace is copied into an ext4 image when the sandbox is created and mounted at `/workspace`. After every command, and before the VM stops, what changed in it is copied back to the host workspace (see "Copying changes back"). Changes made on the host after create do not reach the VM. The root filesystem is shared between VMs and mounted read-only. |
| Inside the guest | Commands run as an unprivileged user (uid 1000), not as root, so they cannot signal or replace the agent that reports their exit codes. The agent and init run as root inside the guest. |
| The VMM process (Linux) | The jailer chroots Firecracker into a folder of its own, runs it as a uid and gid no other VM shares, in a cgroup v2 leaf with CPU, memory (guest memory plus 256 MiB, no swap) and process limits and a file descriptor limit, with Firecracker's seccomp filters. Filters are never turned off; an operator may supply a stricter compiled filter. |
| Devices | Two block devices (root, workspace), vsock, and a serial console whose output is kept only as a short, printable tail for diagnosing a failed boot. No NIC, no shared folders, no GPU. |

Not isolated, or not yet:

- A VM is only as strong as KVM and Firecracker. Keep both patched.
- The guest kernel and root filesystem are the operator's. A guest with
  extra tools, setuid binaries or kernel modules gives commands more to work
  with inside the VM.
- A command can power its own VM off. That ends its own sandbox, nothing else.
- If the driver process is killed, its VMs keep running until it starts
  again. On start, `abhed-microvm` takes its `state_dir`'s lock (a second
  driver on the same `state_dir` refuses to start), then ends every VM a
  killed run of it left and removes their chroots, cgroups and workspace
  images. It recognises its own by a marker it writes beside each VM's
  chroot naming its `state_dir`; a VM of another driver, a folder without
  the marker, or a link, is left alone. Changes in a VM it ends this way
  that were not yet copied back are lost. A macOS helper's VM ends as soon
  as the driver that started it does.
- Snapshots, copy in and out and port forwarding are not implemented; the
  driver declares only `attach`.
- No DNS inside the guest. Names are resolved by the egress proxy for
  requests sent through it; a program that resolves names itself fails.

## Copying changes back

When a command ends, the guest agent sends the host what changed in
`/workspace` since the last copy: the paths deleted, then every file,
folder and symlink created or changed. The guest is untrusted, so the host
checks each entry before it touches anything:

- Only plain relative paths below the workspace. `..`, absolute paths and
  paths not in plain form are refused.
- Every folder on the way is opened without following links. A path through
  a link on the host is refused; a link at the end is replaced, never
  written through. Nothing on another filesystem is entered.
- A file is written beside its destination and renamed over it, so a hard
  link to the old file, inside or outside the workspace, keeps its content.
  Hard links from the guest are not copied back.
- Never written or deleted: `.abhed` at any depth, git's configuration,
  hooks and pointers in any git folder (`config`, `config.worktree`,
  `commondir`, `gitdir`, `hooks/`, `info/attributes`,
  `objects/info/alternates`, and a `.git` file), and the paths the server
  marks write-protected. A folder holding one of these is never removed or
  replaced.
- Symlinks come back only when they point inside the workspace and at
  nothing protected. Devices, pipes and sockets stay in the VM. Permission
  bits come back; setuid, setgid and sticky bits do not.
- Run as root, the driver writes only into folders the workspace's user
  owns, and gives what it creates to that user.

What was refused is listed on the command's stderr, so the agent sees it.
A command whose changes could not be copied back at all fails with an
error, even though it ran. Copies are last-writer-wins: a file edited both
on the host and in the VM ends up as the VM left it.

## Network

The microVM never gets a network device. When the sandbox's policy is
`sandbox.network: "allowlist"`, the control plane gives the driver the
session's egress proxy: its Unix socket and the loopback port commands are
told to use. The driver then:

- listens on one host-side vsock port for the guest (Firecracker's
  `v.sock_1025` in the VM's chroot), made before the VM starts;
- has the guest agent listen on `127.0.0.1:<port>` inside the guest and
  carry each connection over vsock to that port;
- joins each to a new connection to the proxy's socket. Running as root,
  it first checks the socket is served by the server's user, so a socket
  path cannot point the guest at anything else on the host.

Each command's per-call credential comes in its environment
(`HTTP_PROXY`, `HTTPS_PROXY` and the lower-case forms), exactly as on the
process tier, so the allowlist, the per-call credentials and the record of
each decision are the proxy's, unchanged. A request without the call's
credential is refused by the proxy.

With the network off, or `allow_network` without the allowlist, the driver
listens on no port for the guest and the guest forwards nothing. The microVM
driver never opens the network directly.

## Linux requirements

The driver refuses to create a sandbox, with a message naming the first
thing missing, unless all of these hold. Nothing falls back to a weaker tier.

- Linux with KVM: `/dev/kvm` must open for reading and writing.
- cgroup v2 mounted at `/sys/fs/cgroup`.
- Firecracker 1.5 or later and its `jailer`, from a Firecracker release,
  owned by root and writable by no one else.
- The driver running as **root**: the jailer needs root to build the chroot
  and drop to each VM's user. Run it as its own process, `abhed-microvm`,
  and keep the Abhed server unprivileged (below).
- A guest kernel (uncompressed `vmlinux` on x86_64, `Image` on arm64) with
  virtio block, virtio vsock and ext4 built in.
- A root filesystem holding `/bin/sh` and the guest agent at
  `/sbin/abhed-guest` (below).
- `mkfs.ext4` from e2fsprogs 1.43 or later, to build workspace images.
- An unused range of uids and gids for the VMs (`uid_base`, `gid_base`,
  `slots`).

## Running it

`abhed-microvm` serves the driver on a Unix socket that only the server's
user can open. It runs as root; workspace images are built as the client's
user, so a sandbox can only be given files that user can read.

```json
{
  "kernel": "/var/lib/abhed-microvm/vmlinux",
  "rootfs": "/var/lib/abhed-microvm/rootfs.ext4",
  "state_dir": "/srv/abhed-microvm/state",
  "vcpus": 1, "max_vcpus": 4,
  "memory_mib": 512, "max_memory_mib": 4096,
  "workspace_mib": 2048,
  "firecracker": {
    "binary": "/usr/local/bin/firecracker",
    "jailer": "/usr/local/bin/jailer",
    "chroot_base": "/srv/abhed-microvm/jail",
    "uid_base": 900000, "gid_base": 900000, "slots": 64
  }
}
```

```sh
sudo install -d -m 0711 /run/abhed-microvm
sudo abhed-microvm -config /etc/abhed-microvm.json \
  -socket /run/abhed-microvm/driver.sock \
  -client-uid "$(id -u abhed)" -client-gid "$(id -g abhed)"
```

Keep `state_dir` and `chroot_base` on one filesystem, so images are linked
into each chroot rather than copied. `state_dir` is mode `0711`: the
client's user passes through it to build an image in a folder of its own,
and can list and read nothing else there. The kernel and root filesystem must be
readable by every user, since every VM's user reads them. Unknown keys in
the file are refused.

A sandbox may ask for fewer resources than the maxima through its template's
driver config, `vcpus` and `memory_mib`; any other key is refused.

## The guest agent and root filesystem

`cmd/abhed-guest` is the agent inside the guest, a static Go binary. The
kernel starts it as init: it mounts `/proc`, `/sys`, `/dev`, `/dev/pts`,
`/tmp` and `/run`, brings up loopback, mounts the workspace image at
`/workspace`, then listens on vsock port 1024. Each connection carries one
call: run a command (exit code, stdout and stderr), run a shell on a
terminal (with resize), a liveness check, or power off. If the agent ends or
setup fails, the guest powers off, so the host sees the VM exit instead of a
guest it cannot reach.

`scripts/microvm/build-rootfs.sh` builds a root filesystem from a base tree
you supply, for example an Alpine minirootfs whose checksum you have
verified, with the agent installed:

```sh
sudo scripts/microvm/build-rootfs.sh -b alpine-minirootfs.tar.gz -o rootfs.ext4 -a amd64
```

## macOS

Not available in this release. Running a VM on Virtualization.framework
needs a helper program built against the framework and code-signed with the
`com.apple.security.virtualization` entitlement; Abhed does not ship one.
Configured with `"helper": {}`, the driver refuses every sandbox and says
exactly that. The protocol a helper must speak is defined in
`driver/microvm/helper.go`: a `check` command, and `run --config` that boots
a VM with no network device and bridges a vsock port to a Unix socket with
the same handshake Firecracker uses. On macOS the `process` tier (Seatbelt)
is unchanged.

## Testing on KVM

The unit tests run everywhere. `TestFirecrackerOnKVM` boots a real microVM
and runs only when `ABHED_TEST_FIRECRACKER=1`, as root, on a host with KVM
(no nested virtualization inside another VM unless the host enables it):

```sh
sudo -E ABHED_TEST_FIRECRACKER=1 \
  ABHED_TEST_FC_BINARY=/usr/local/bin/firecracker \
  ABHED_TEST_FC_JAILER=/usr/local/bin/jailer \
  ABHED_TEST_FC_KERNEL=$PWD/vmlinux \
  ABHED_TEST_FC_ROOTFS=$PWD/rootfs.ext4 \
  go test -run TestFirecrackerOnKVM -count=1 ./driver/microvm/
```

It checks that the workspace arrives, exit codes come back, commands run as
the unprivileged user and cannot signal init, the guest has no interface
but loopback, the root filesystem is read-only, the workspace survives a
restart, and the chroot and cgroup are gone after delete. A CI job for it
needs a runner with `/dev/kvm`, a pinned Firecracker release, a pinned guest
kernel, and the root filesystem built by the script above.
