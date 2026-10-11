# Container drivers

The `docker` and `podman` drivers run each sandbox as one long-lived
container. They implement the driver interface in
[the control-plane contract](../architecture/control-plane-contract.md), in the
Go package `driver/container`, and are an experimental API until 2.0.
`abhed serve` registers them when `control_plane.docker` or
`control_plane.podman` is enabled ([Sandboxes over the
API](22-sandboxes.md#the-docker-and-podman-drivers)), for `/v1/sandboxes`
and for [per-session sandboxes](22-sandboxes.md#per-session-sandboxes).

They are separate from the container **tier** that `sandbox.min_tier:
container` selects today, which starts a fresh container for every command.
That tier is unchanged.

```go
d, err := container.NewPodman(cfg, container.Options{})   // or NewDocker
st, err := d.Create(ctx, driver.Spec{ID: "sbx_1", Workspace: "/home/me/project"})
res, err := d.Exec(ctx, "sbx_1", driver.ExecRequest{Command: "make test", Stdout: os.Stdout})
```

Each sandbox's policy is built from the configuration exactly as a
session's is (`sandbox.*`, `egress`, the state paths), and the options and
the sandbox's own settings can only narrow it.

## What every container gets

| Flag | Why |
|---|---|
| `--cap-drop ALL`, `--security-opt no-new-privileges` | no capability, and no setuid way back to one |
| the engine's default seccomp profile | `Options.SeccompProfile` may name another file; `unconfined` is refused |
| `--read-only`, `--tmpfs /tmp` and `/run` (noexec, nosuid, sized) | the image cannot be changed and scratch space cannot run what is written there; on Podman `--read-only-tmpfs=false` too |
| `--network none` | unless `sandbox.allow_network` is on; under the allowlist, see below |
| `--memory` and the same `--memory-swap`, `--pids-limit`, `--cpus` | from `sandbox.max_memory_mb`, `sandbox.max_procs` and `Options.CPUs` (2 by default) |
| `--ulimit` on file size, open files and core dumps | as the container tier |
| `--ipc private`, `--hostname abhed`; on Podman `--pid private --uts private` | nothing shared with the host or another sandbox |
| `--init` | reaps what commands leave behind |
| `--user` this user's uid:gid | never root; when Abhed runs as root, the workspace's owner, else 65534. Rootless Podman adds `--userns keep-id` |
| the workspace at the same path, read-write | the only writable host folder |
| `.abhed` and the state paths covered, `read_only_paths` read-only, `write_protected` and git folders read-only | the same mounts as the container tier |

After the container starts, the driver reads `/proc/self/status` and
`/proc/net/dev` from inside it. If the kernel reports no seccomp filter,
`no_new_privs` off, any capability, a different uid, a network device other
than `lo` where there should be none, or a writable root filesystem, the
container is removed and `Create` fails. `Status.Tier` is `container` only
for a container that passed this check.

One exception to the network rule: the fallback tunnel devices the kernel
puts in every network namespace once the ipip, sit, ip6_tunnel, gre or vti
modules are loaded (`tunl0`, `sit0`, `ip6tnl0`, `gre0`, `gretap0`,
`erspan0`, `ip_vti0`, `ip6_vti0`, `ip6gre0`, common on Calico and Flannel
nodes) are accepted when `/sys/class/net` shows the device down and not up,
it holds no IPv6 address, and the namespace has no IPv4 address beyond
loopback. Any of them up or addressed, or a state the driver cannot read,
still removes the container.

Under rootless Docker, root inside the container is your own user outside,
and the only uid that can write the workspace, so commands run as uid 0
there, with no capabilities. `Status.Message` says so.

## Tiers

A container is tier `container`. A sandbox whose `Spec.MinTier`, or whose
`sandbox.min_tier`, is `vm` is refused with `unavailable`, and so is
`sandbox.tier: fence`: the driver never builds something weaker than asked.

## Images

The default image is `ABHED_SANDBOX_IMAGE`, as for the container tier, or
`Options.Image`. A sandbox may ask for another with `Config["image"]` only
when it is listed in `Options.Images`. The image's own entrypoint is not
run; the image needs `/bin/sh` and `sleep`.

## Extra mounts

`Options.Mounts` are folders every sandbox gets. A sandbox may ask for its
own with `Config["mounts"]`, a JSON list of `{"host", "target", "rw"}`, but
only from under a folder in `Options.MountRoots`, and writable only when that
root says so. Mounts are read-only unless asked otherwise.

Every mount is judged with links followed, so a link inside a root that
leads out of it is refused. Refused whatever the configuration says:

- `/`, `/proc`, `/sys`, `/dev`, `/run`, `/var/run`, and the engines' state
  folders;
- any Unix socket, device or pipe; the engines' sockets and the folders that
  hold them (`DOCKER_HOST`, `CONTAINER_HOST`, `XDG_RUNTIME_DIR`, `~/.docker`
  and the like);
- Abhed's state: `~/.abhed`, the workspace's `.abhed`, `/etc/abhed`, the
  configured state paths, and any folder holding one of them;
- a home folder whole, or a folder holding one;
- a target over `/`, `/proc`, `/sys`, `/dev`, `/run`, `/tmp` or the
  workspace;
- a path with `:` or `,`, which a mount argument cannot carry.

On an SELinux host the workspace and what is mounted from inside it are
relabelled with `:z`, a label every container may share, so two sandboxes
on one workspace both work. An extra mount is relabelled only when its
`Mount.Relabel` or its root's `Relabel` says so; system folders such as
`read_only_paths` and `/dev/null` never are.

## GPUs

`Options.GPUs` lists CDI device names a sandbox may ask for, such as
`nvidia.com/gpu=all`; none by default. A sandbox asks with `Config["gpus"]`,
and gets `--device <name>` for each. A raw device path is refused. The
engine must have the vendor's CDI specification installed.

## Resources

`Config["memory_mb"]`, `Config["pids"]` and `Config["cpus"]` may lower the
configured bounds, never raise them. Any other key in `Config` is refused.

## Network

With the network off, the container has only `lo`. With
`sandbox.allow_network` on, it has the engine's default network.

Under `sandbox.network: "allowlist"` the egress proxy stays on the host and
applies the same `egress` rules as the process tier. The container still has
no network: its first process is a relay, bound in from the host, that
listens on the container's own loopback and joins each connection to the
proxy's Unix socket, also bound in. Each command gets a credential of its
own in `HTTP_PROXY` and `HTTPS_PROXY`, passed to the engine by name so its
value is never in an argument, and revoked when the command ends. Each
decision goes to `Options.Record` as an `egress.decision` event with the
sandbox's id. Names are resolved by the proxy, so tools must honour the
proxy variables; there is no resolver inside.

Where the engine runs in a VM (Docker Desktop, Podman on macOS and
Windows) or elsewhere (`DOCKER_HOST=tcp://…`), a host socket cannot reach
the container. There the relay is a Linux build of Abhed for the engine's
architecture that you name in `control_plane.<engine>.relay_binary`
(managed only), at an absolute path the engine's host can see (under your
home folder for Podman's and Docker Desktop's VMs, which share it). It is
bound in read-only, and the driver runs it with a long-lived `exec` as the
sandbox's user: it listens on the container's loopback and carries each
connection over that exec's input and output to the proxy on this host.
The container still has no network. The sandbox is ready only once the
relay listens; an exec that ends is started again while the sandbox runs,
and connections made meanwhile fail rather than go anywhere else. Without
`relay_binary`, `Create` fails with `unavailable` rather than run without
the allowlist.

## Lifecycle

| Call | What happens |
|---|---|
| `Create` | checks the spec and the mounts, starts the container, verifies it |
| `Exec` | `exec` as the sandbox's user, in a folder inside the workspace; `Env` may add variables but not replace `PATH`, `HOME`, `ABHED_SANDBOX`, the proxy variables or `LD_*` |
| `Attach` | an interactive shell on a terminal, with resize |
| `Command` | the same `exec`, or the shell, handed to the caller to run: a session's bash tool and terminal; under the allowlist its credential is revoked when the caller's context ends, and a cancelled one is ended inside the container |
| `CopyIn`, `CopyOut` | a tar stream through the engine's `cp`, to or from a folder inside the workspace; every entry is checked (no absolute names, no `..`, no links out, no devices), owners become the sandbox's user and setuid bits are dropped |
| `Stop` | refuses new commands, then stops the container with the grace given |
| `Start` | a fresh container from fresh mounts; `/tmp` starts empty, the workspace is as it was |
| `Forward` | a byte stream to a port on the container's loopback, through `exec`; the container gets no network for it |
| `Snapshot`, `Restore` | with `Options.SnapshotDir` set: the workspace archived and the container committed; see [snapshots](25-snapshots-and-forwarding.md) |
| `Delete` | removes the container, the egress proxy, and the sandbox's snapshots and their images |

A cancelled command is ended inside the container too: its wrapper leaves
its pid in `/tmp`, and the driver stops and kills it and every descendant.

With git protection on, the git folders are mounted read-only when the
container starts. A repository made after that cannot be mounted into a
running container, so commands are refused with `conflict` until the
sandbox is restarted; the restart protects it.

A container this process did not create, such as one left by a crash, is
reported by `Status` without a tier and removed by `Delete` when its labels
say it is this driver's sandbox of that id. `Create` removes such a
container first when it is stopped, as a session reopened after the server
restarted finds one; one still running, or one whose labels name another
driver or id, keeps its name, and `Create` fails with `exists` without
touching it.

Forwarding a port is always declared; snapshots only with
`Options.SnapshotDir`. Both are in
[Snapshots and forwarding](25-snapshots-and-forwarding.md).

## Testing on a real engine

```sh
ABHED_REQUIRE_CONTAINER_DRIVER=podman go test ./driver/container/ -run OnARealEngine -v
```

The tests start containers of their own, named with a test prefix, and remove
them. CI runs them
on Docker and Podman on Linux, where the allowlist path runs too.
