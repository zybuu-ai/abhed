# The microVM driver

The `microvm` driver runs each sandbox in a small virtual machine with its own
guest kernel. On Linux the VM is a Firecracker microVM started by
Firecracker's jailer on KVM; on macOS it is a VM on Apple's
Virtualization.framework, run by `abhed-vmhelper`, a separate signed helper
(see "macOS" below).
Instances report the isolation tier `microvm`.

This is hardware virtualization, and it is a different thing from the `vm`
sandbox tier. The `vm` tier is gVisor: a user-space kernel in front of a
container, sharing the host kernel. Neither is described as the other
anywhere in Abhed.

**Status: experimental.** The driver implements the driver interface
(`docs/architecture/control-plane-contract.md`) and is served to the Abhed
server over the out-of-process driver protocol. When the managed
configuration sets `sandbox.microvm_socket`, the driver registry Abhed
builds from its configuration includes `microvm`, so a sandbox can choose it
by name; the default stays `process`. The driver's process handling, files and wire are tested
on every platform against a stand-in VMM, real microVMs are booted on
KVM in CI (see "Testing on KVM"), and on a Mac by a test run there (see
"Testing on a Mac").

## What is isolated, and what is not

| | |
|---|---|
| Kernel | The guest runs its own Linux kernel. A command that exploits a kernel bug compromises the guest kernel, not the host's. |
| Network | The VM has **no network device**, ever. The only interface in the guest is loopback. With the network off, the host listens on no port for the guest and the guest forwards nothing: there is no path out. With the egress allowlist on, the session's egress proxy is the only way out (see "Network"). |
| Host channel | One vsock connection to the guest agent. On Linux the host side is a Unix socket in the VM's chroot; the driver opens it without following a symlink, so a compromised VMM cannot point it elsewhere. |
| Files | The workspace is copied into an ext4 image when the sandbox is created and mounted at `/workspace`. After every command, and before the VM stops, what changed in it is copied back to the host workspace (see "Copying changes back"). Changes made on the host after create do not reach the VM. The root filesystem is shared between VMs and mounted read-only. |
| Inside the guest | Commands run as an unprivileged user (uid 1000), not as root, so they cannot signal or replace the agent that reports their exit codes. The agent and init run as root inside the guest. |
| The VMM process (Linux) | The jailer chroots Firecracker into a folder of its own, runs it as a uid and gid no other VM shares, in a cgroup v2 leaf with CPU, memory (guest memory plus 256 MiB, no swap) and process limits and a file descriptor limit, with Firecracker's seccomp filters. Filters are never turned off; an operator may supply a stricter compiled filter. |
| Devices | Two block devices (root, workspace), vsock, and a console whose output is kept only as a short, printable tail for diagnosing a failed boot; on macOS also an entropy device. No NIC, no shared folders, no GPU. |

Not isolated, or not yet:

- A VM is only as strong as KVM and Firecracker, or on macOS as
  Virtualization.framework. Keep them patched.
- The guest kernel and root filesystem are the operator's. A guest with
  extra tools, setuid binaries or kernel modules gives commands more to work
  with inside the VM.
- A command can power its own VM off. That ends its own sandbox, nothing else.
- If the driver process is killed, its VMs do not outlive it. On Linux
  each VM has a watchdog, a small process of the driver's own binary in a
  session of its own, that waits on a pipe only the driver holds; when the
  driver ends without stopping the VM itself, the watchdog kills the VM's
  cgroup. A macOS helper's VM ends as soon as the driver that started it
  does. On start, `abhed-microvm` takes its `state_dir`'s lock (a second
  driver on the same `state_dir` refuses to start), then removes what a
  killed run of it left (chroots, cgroups, workspace images) and ends any
  VM still there. It recognises its own by a marker it writes beside each
  VM's chroot naming its `state_dir`; a VM of another driver, a folder
  without the marker, or a link, is left alone. Changes in a VM ended this
  way that were not yet copied back are lost.

## Copies, forwards and snapshots

The driver declares `attach`, `copy` and `forward`, and `snapshot` when
`snapshot_dir` is set (a folder outside every workspace).

- **Copy in** streams the archive to the guest agent, which checks every
  entry as a copy is checked (no absolute names, no `..`, no links out, no
  devices, nothing in `.abhed`), writes it into the folder under
  `/workspace` it was given, owned by the commands' user, and never over an
  existing file. The copy then comes back to the host workspace as a
  command's changes do. A path may be relative to the workspace or absolute
  in it (the host's path or `/workspace/…`).
- **Copy out** first copies the guest's changes back, then packs the file or
  folder from the host workspace under its own name.
- **Forward** asks the guest agent to connect to the port on the guest's own
  loopback and carries the bytes over vsock; the VM still has no network
  device. A stream ends when it is closed or the sandbox stops, and each is
  recorded (`sandbox.forward`) when the driver has a record.
- **Snapshot** copies the guest's changes back and archives the host
  workspace, without `.abhed`, by its SHA-256. **Restore** needs the
  sandbox stopped, checks the digest, replaces the host workspace and builds
  the workspace image again, so the next start boots with it.

These need an `abhed-guest` from this release in the root filesystem; an
older guest refuses them, and the call fails with that reason.

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
  marks write-protected (`abhed serve` passes its own, the same ones its
  sessions' commands may not write). A folder holding one of these is never removed or
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
`sandbox.network: "allowlist"`, the control plane gives the driver an
egress proxy: its Unix socket and the loopback port commands are told to
use. `abhed serve` starts one per microVM sandbox, serving the configured
allowlist, records each decision with the sandbox's id, and stops it when
the sandbox is deleted; a sandbox whose proxy cannot start is not created.
A client of `:exec` cannot set the proxy variables itself. The driver then:

- listens on one host-side vsock port for the guest (Firecracker's
  `v.sock_1025` in the VM's chroot), made before the VM starts;
- has the guest agent listen on `127.0.0.1:<port>` inside the guest and
  carry each connection over vsock to that port;
- joins each to a new connection to the proxy's socket. Running as root,
  it first checks the socket is served by the server's user, so a socket
  path cannot point the guest at anything else on the host.

The guest also gets a resolver. `abhed serve` serves the proxy's resolver
on a second socket and gives the driver a DNS credential of the sandbox's
own; the driver listens on a second vsock port (`v.sock_1026`), and the
guest agent answers on `127.0.0.1:53` (UDP and TCP), with
`/etc/resolv.conf` pointed at it (a file on `/run` bound over the read-only
root's, which must exist). Each lookup is carried to the driver, which adds
the credential, the guest never seeing it, and passes it to the proxy's
resolver. The same egress rules judge it: a name the allowlist does not
allow is `NXDOMAIN`, an allowed one gets one of the proxy's own addresses,
which the proxy maps back to the name when a command connects through it,
and each decision is recorded as the proxy's other decisions are. A
program that resolves a name and then connects without the proxy still has
no route out. A guest built before this release has no resolver; the
driver logs it and names then do not resolve inside.

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

## Getting the binaries

Each release carries `abhed-microvm-<version>-linux-<arch>` and
`abhed-guest-<version>-linux-<arch>` for `amd64`, `arm64` and `s390x`, beside
the `abhed` archives and listed in the same `SHA256SUMS`:

```sh
sha256sum -c SHA256SUMS --ignore-missing
sudo install -m 0755 abhed-microvm-<version>-linux-amd64 /usr/local/bin/abhed-microvm
```

Firecracker itself runs on `x86_64` and `aarch64` only, so on `s390x` the
driver refuses every sandbox; those two binaries are there for building and
testing. The guest kernel and the root filesystem are not shipped: build the
root filesystem with the release's `abhed-guest` (below), and take a guest
kernel from a source you trust, checking its checksum as the CI job does.
A root filesystem is not reproducible byte for byte (`mkfs.ext4` stamps a
fresh UUID and times), so record the checksum of the one you built.

Releases do not carry `abhed-vmhelper`, or `abhed-microvm` for macOS, yet:
the helper has to be signed and notarized with a Developer ID before it can
be handed out. On a Mac, build both yourself ([macOS](#macos)).

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

Then point Abhed at it in the managed configuration
(`/etc/abhed/config.json`):

```json
{ "sandbox": { "microvm_socket": "/run/abhed-microvm/driver.sock" } }
```

`abhed doctor` then shows `microvm ready`, or `UNAVAILABLE` with the reason,
and is not ready while the driver cannot be reached.

Keep `state_dir` and `chroot_base` on one filesystem, so images are linked
into each chroot rather than copied. `state_dir` is mode `0711`: the
client's user passes through it to build an image in a folder of its own,
and can list and read nothing else there. The kernel and root filesystem must be
readable by every user, since every VM's user reads them. Unknown keys in
the file are refused.

A sandbox may ask for resources up to the maxima through its driver config:
`vcpus` and `memory_mib`, or the control plane's sizes (`resources.cpus`,
rounded up to whole vCPUs, and `resources.memory_mb`), which reach the
driver as `cpus` and `memory_mb`, as they do the container drivers. Any
other key is refused.

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
# or with the release's agent instead of building one:
sudo scripts/microvm/build-rootfs.sh -b alpine-minirootfs.tar.gz -o rootfs.ext4 \
  -g abhed-guest-<version>-linux-amd64
```

## macOS

On macOS the VM runs on Apple's Virtualization.framework, through
`abhed-vmhelper`: a small Swift program in `macos/abhed-vmhelper`, built
with the Xcode command-line tools and no third-party packages. The
framework runs only a program code-signed with the
`com.apple.security.virtualization` entitlement, which is why the helper is
a binary of its own and the Go driver is not.

What the VM gets, and nothing more:

| | |
|---|---|
| Boot | The configured kernel and command line (`VZLinuxBootLoader`), no initrd. |
| Disks | The root filesystem, read-only, and the workspace image, as virtio block devices. |
| Host channel | One virtio vsock device. The helper listens on a Unix socket (mode `0600`) in the instance's state folder and joins each `CONNECT <port>` from the driver to that guest port, the handshake Firecracker uses. A guest connection is accepted only on the egress port the driver asked for, and joined to the driver's socket for it; every other port is refused. |
| Console | A virtio console the guest writes to and the helper copies to its standard output, which the driver keeps as a short, printable tail. Nothing is ever sent to the guest on it. |
| Entropy | A virtio entropy device, so the guest's random numbers are ready at boot. |
| Never | A network device, shared folders, clipboard, sound, USB, graphics, keyboard, pointer or memory balloon. The helper sets each of these lists to empty rather than relying on defaults, and refuses a configuration with a key it does not know or a `network` other than `"none"`. |

CPUs and memory are the sandbox's, within the operator's maxima, and the
helper refuses more than the Mac allows (at most the Mac's CPU count and
half its memory). The helper exits, stopping the VM, when the guest powers
off, when it is sent `SIGTERM`, `SIGINT` or `SIGHUP`, or when its standard
input ends. The driver holds the only write end of that input, so a driver
that dies, even by `SIGKILL`, takes its VMs with it. Exit status 0 means the
VM stopped, 1 that it could not start or failed, 2 that the helper refused
its command line or configuration and ran nothing.

### Requirements

- macOS 13 or later, on Apple silicon (an Intel Mac with VT-x can run the
  helper, but needs an x86_64 guest kernel and root filesystem, which are
  not provided or tested).
- `abhed-vmhelper`, signed with the entitlement and the hardened runtime,
  beside `abhed-microvm`, or wherever `helper.path` says.
- An arm64 guest kernel as an uncompressed `Image`, with virtio PCI, block,
  console and vsock built in, and an arm64 root filesystem holding the
  guest agent (both below).
- `mkfs.ext4` from e2fsprogs 1.43 or later, to build workspace images.
  macOS does not ship it; `brew install e2fsprogs` installs it under
  `$(brew --prefix e2fsprogs)/sbin`, which `mkfs_ext4` can name.
- Not root: the helper and `abhed-microvm` run as the user.

Before it runs the helper, the driver checks it with `/usr/bin/codesign`
and refuses it, with a message naming what is wrong, unless it:

- is signed, and unchanged since it was signed;
- carries the identifier `com.zybuu.abhed.vmhelper`;
- holds the `com.apple.security.virtualization` entitlement;
- was signed with the hardened runtime;
- when `helper.team_id` is set, was signed with a Developer ID certificate of
  that Apple Developer team.
- sits in a folder only its owner may write, below folders no one else may
  write (a sticky shared folder such as `/tmp` excepted), so it cannot be
  swapped after it is checked.

It then runs `abhed-vmhelper check`, which answers whether this Mac can run a
VM and whether the helper holds the entitlement. Without a helper, the driver
refuses every sandbox and says where it looked. On macOS the `process` tier
(Seatbelt) is unchanged either way.

### Building the helper

```sh
scripts/macos/build-vmhelper.sh -o bin/abhed-vmhelper
```

builds it with `swift build` and signs it ad hoc with its entitlement and the
hardened runtime. That is enough on the Mac that built it; leave
`helper.team_id` unset to accept it. An ad-hoc signature says nothing about
who built the program, so set `team_id` wherever the helper comes from
someone else.

A helper for others to download must be signed with a Developer ID and
notarized by Apple, or macOS refuses to run it.
`scripts/macos/sign-vmhelper.sh` does that, with these from the
environment:

- `ABHED_SIGN_IDENTITY`: a "Developer ID Application" certificate in the
  keychain (from an Apple Developer Program membership), by name or SHA-1;
- `ABHED_NOTARY_PROFILE`: a `notarytool` keychain profile, made once with
  `xcrun notarytool store-credentials` and an app-specific password;
- `ABHED_SIGN_KEYCHAIN`, optionally, the keychain holding the identity.

It signs with the hardened runtime and a secure timestamp, notarizes a disk
image holding the helper and staples the ticket to it (a bare executable
cannot hold one), and prints the team id for `helper.team_id`.

### The guest

```sh
scripts/macos/guest-image.sh -o ~/abhed-guest
```

fetches the arm64 guest kernel Firecracker's CI publishes (Linux 6.1, an
uncompressed `Image` with virtio PCI, block, console and vsock built in, so no
initrd or modules are needed) and an Alpine minirootfs, checks each against a
SHA-256 pinned in the script, builds the guest agent for arm64, and runs
`scripts/microvm/build-rootfs.sh` in a throwaway Linux container (podman or
docker), since it needs Linux and root. The same kernel series is the one
the KVM tests boot on x86_64.

### Running it

```json
{
  "kernel": "/Users/me/abhed-guest/vmlinux-arm64",
  "rootfs": "/Users/me/abhed-guest/rootfs-arm64.ext4",
  "state_dir": "/Users/me/.local/state/abhed-microvm",
  "mkfs_ext4": "/opt/homebrew/opt/e2fsprogs/sbin/mkfs.ext4",
  "vcpus": 1, "max_vcpus": 2,
  "memory_mib": 512, "max_memory_mib": 2048,
  "helper": {}
}
```

```sh
abhed-microvm -config ~/abhed-microvm.json -socket ~/.local/state/abhed-microvm.sock
```

`"helper": {}` uses `abhed-vmhelper` beside `abhed-microvm`; `"path"` names
another, and `"team_id"` the Developer ID team it must be signed by. Keep
`state_dir` short: a Unix socket path inside it is limited to about 100
bytes.

## Testing on a Mac

`TestVZHelperWithoutBooting` runs the built helper without starting a VM: its
signature, `check`, and the configurations and command lines it must refuse.
`TestVZHelperBootsAMicroVM` boots real VMs, one vCPU and 256 MiB each, and
checks that commands run with their exit codes, as the unprivileged user, in
an arm64 guest with no interface but loopback and no shared folder, on a
read-only root; that workspace changes come back and never into `.abhed`;
that the workspace survives a restart; that the egress proxy is the only way
out; that a helper whose standard input ends stops its VM and exits 0; and
that no helper process outlives its instance.

```sh
scripts/macos/build-vmhelper.sh -o bin/abhed-vmhelper
scripts/macos/guest-image.sh -o /tmp/abhed-guest
ABHED_TEST_VZ=1 ABHED_TEST_VZ_HELPER=$PWD/bin/abhed-vmhelper \
  ABHED_TEST_VZ_KERNEL=/tmp/abhed-guest/vmlinux-arm64 \
  ABHED_TEST_VZ_ROOTFS=/tmp/abhed-guest/rootfs-arm64.ext4 \
  ABHED_TEST_VZ_MKFS="$(brew --prefix e2fsprogs)/sbin/mkfs.ext4" \
  go test -run 'TestVZHelper' -count=1 ./driver/microvm/
```

Set `ABHED_TEST_VZ_TEAM_ID` to test a Developer ID signed helper against its
team. CI's `macos-vmhelper` job builds the helper, signs it ad hoc and runs
the tests that do not boot, failing if any of them did not run. GitHub's
hosted macOS runners are themselves VMs without nested virtualization, so
the framework cannot start a VM there; the job prints the helper's `check`
answer and says the boot test was not run, rather than skipping it quietly.

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
but loopback, the root filesystem is read-only, changes come back as the
workspace's user and never into `.abhed`, the workspace survives a restart,
a VM reaches an allowed destination only through the egress proxy and
under the call's credential, a restarted driver ends the VMs a killed one
left, and the chroot and cgroup are gone after delete.

CI runs it in the `microvm-kvm` job on `ubuntu-24.04`, which fails if the
runner has no `/dev/kvm`. It fetches a pinned Firecracker release, a pinned
guest kernel from Firecracker's CI artifacts and a pinned Alpine
minirootfs, checks each against its SHA-256, and builds the root
filesystem with `scripts/microvm/build-rootfs.sh`.
