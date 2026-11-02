# Debugging a program in the sandbox

Abhed Studio's Run and Debug views start a debugger through the engine, not on
your machine. The debug adapter, and the program it runs, live in the
session's sandbox, as a `bash` command would: the same mounts, the same
network rules, the same policy. Studio sees the session through the engine,
which carries the Debug Adapter Protocol (DAP) messages both ways. No port
is opened on the host for it.

Supported today:

| Language | Adapter, in the sandbox | What can be debugged |
|---|---|---|
| Go | `dlv dap` (Delve) | a program (`dlv debug`) or a package's tests (`dlv test`) |
| Python | `python3 -m debugpy.adapter` | a program, or tests run with `pytest` |

Node is not supported yet: its adapter (js-debug) is a separate install, and
comes with the toolchain manager.

## Where it runs

A debug adapter talks to the program it debugs over loopback, so it needs a
sandbox with a network of its own:

- **The container tier** (Docker or Podman; on a Mac, the engine's VM) and the
  `vm` tier.
- **The Linux process tier, with the sandbox's network closed or
  allowlisted.** Bubblewrap then gives each command its own network
  namespace (`--unshare-net`). With the network open
  (`sandbox.allow_network: true` and no allowlist) it does not, and the
  adapter's loopback would be the host's, so a launch is refused with steps:
  close the network, use the allowlist, or use the container tier.

Not the macOS process tier as it is: its loopback is the host's, and opening
it lets the program reach the services on your machine. A launch there (or
on the Linux process tier with the network open, or with no sandbox) is
refused with a `container-setup` problem that says what the container tier
needs on this machine and the exact steps, which Studio walks you through:

1. Install Podman, where none is found: `brew install podman` where
   Homebrew is (on Linux, your package manager; else podman.io).
2. On a Mac, create and start its virtual machine: `podman machine init`,
   `podman machine start` (only the start when the machine exists).
3. Run this workspace's commands in a container: Studio writes
   `{"sandbox": {"min_tier": "container"}}` to the workspace's
   `.abhed/config.json` (a workspace file may always raise the tier), and
   you open a new session.

`_abhed/debug/status-tier` gives the same at any time: the tier, whether a
debug session can start, the container engine, Homebrew and the machine
found, and the steps left.

**Or debug on the process tier, once you agree.** If you would rather not
set up containers, Studio can launch on the process tier instead. Until you
allow it for the workspace, each such launch asks, with what it means: on
macOS, while it is being debugged, the program can listen on this Mac's
loopback ports, where other programs on this Mac can reach it, and a Go
program can also reach services on this Mac (localhost ports); nothing
else on the network. On Linux, where this only applies with the session's
network open, it can reach those services and the network, as the
session's commands can. **Allow for this workspace** keeps the choice (in
`~/.abhed/state/debug-process-tier.json`; Studio's forget,
`_abhed/debug/forget-consent`, takes it back), and later launches there are
not asked about it. **Allow once** starts this debug session only and keeps
nothing. Deny, a cancel or no answer starts nothing. The question and your
answer are recorded with the launch, a kept choice as `debug.setup`
(`process_tier_consent`), and each such session's `debug.launched` has
`loopback: true`.

On macOS only the debug command gets loopback, and only what its debugger
needs; the agent's commands keep the network closed:

- **Abhed's reserved port range.** Every Abhed listener on a Mac that does
  not name its port (the egress proxies and each command's own proxy port,
  `abhed sandbox forward` without a local port, `abhed serve`, the metrics
  listener and `abhed egress-proxy` on port 0, the sign-in callback) takes
  its port from one range, 47600 to 47855 unless `ABHED_LOOPBACK_PORTS`
  names another (`lo-hi`, at least 16 ports above 1024; give every Abhed
  process on the Mac the same one: a value that is not valid is warned
  about and the default used). A listener that finds the range full fails,
  saying so; it never takes another port. The range holds 256 ports,
  shared by every Abhed on the Mac: each session's proxy takes one, and
  each command running at that moment another, so a very large parallel
  run can fill it (set a wider range then). The rule for a debug command
  is: never the reserved range. So no Abhed proxy or listener of any
  engine on the Mac is reachable from it, whatever the session's network.
  A port you name yourself (`abhed serve -addr 127.0.0.1:52000`, a forward
  to `52000:REMOTE`) is taken as named: one the system also hands out
  (49152 to 65535) is reachable from a Go program being debugged.
- **Python:** debugpy's adapter is run with the ports it listens on chosen
  by the engine before it starts, outside the reserved range, five of them
  (its listener, its launcher's, and one for each of three restarts of the
  session). The adapter binds all of them on the wildcard and loopback
  addresses, IPv4 and IPv6, before anything else and holds them until it
  exits, so no other program can take one of those; the command's Seatbelt
  profile lets it connect to, bind and accept on those ports alone.
  Seatbelt's "localhost" also matches the Mac's own network addresses, so
  the program may listen on those five ports on any address of this Mac,
  where other machines on the network can reach it; the consent says so.
  It may listen on no other port unless commands in this session may
  listen ([`sandbox.listen`](02-configuration.md), or you agreed for the
  workspace): then, as any command, it may listen on any port but Abhed's
  reserved range and the ports the Mac already listens on, on any of the
  Mac's addresses, which other machines on the network can reach. So a
  program that serves on a port of its own (a Flask or Django dev server,
  say) can be debugged under Python on this Mac's process sandbox only with
  listening on; otherwise use the container tier for it.
- **Go:** Delve runs natively (it needs the Xcode command line tools'
  debugger), bridged by the engine's own program through a socket folder
  the engine makes for the session in `~/.abhed/run`, which every other
  command's profile denies. Delve picks its own loopback ports, which the
  system hands out (49152 to 65535, `net.inet.ip.portrange`), so the
  command may connect to each of those but the reserved range, one
  Seatbelt rule per port: Seatbelt does not reliably let a later deny carve
  one port out of `localhost:*`, so the rules never use it. Starting such a
  command takes a few seconds while Seatbelt reads them. It may listen on
  any port on any address of the Mac, where other machines on the network
  can reach it (Seatbelt's "localhost" matches those too): the consent says
  so. Never on a port of the reserved range, on any address, so it cannot
  take an Abhed listener's port or fill the range; and while commands may
  listen (`sandbox.listen`), not on a port the Mac already listens on
  either.
- **Older Abhed on the same Mac.** Abhed releases before 1.2.9 do not use
  the reserved range: their egress proxies and each command's own proxy
  port take ports the system hands out, which a Go program being debugged
  here can reach, and a command's own proxy port carries that command's
  allowlist. Stop older Abhed processes on the Mac (or use the container
  tier) while debugging Go on this Mac's process sandbox.
- A launch the sandbox would still refuse (every loopback port while the
  session's commands go out through the egress proxy) is a problem with
  steps, and an "Allow for this workspace" given for it is not kept: the
  agreement is kept only once the session it was given for has started.

Studio's "use the container tier" step never lowers a workspace: one
already at `container` or `vm` is left as it is (`already: true`).

On the Linux process tier the session's commands share one `/tmp`, so
Delve's socket is put in a folder only the debug command sees (an empty
filesystem in its own mount namespace, named afresh for each launch), not
where the agent's other commands could connect to it first.

The debugger must be in the sandbox: Delve for Go, `debugpy` for Python.
The simplest way is to have Abhed install the language with its debugger
([Toolchains: Debuggers](32-toolchains.md#debuggers)): accept Studio's
offer, or run `abhed toolchain install -debugger go` (or `python`; add
`-platform linux` for the container tier), and the next launch finds it on
`PATH`. Delve is built for Linux and for macOS (the process tier). A Go
launch without Delve where the session's commands run is refused before
anything is judged, with an `adapter-missing` problem whose `install`
(`{language: "go", debugger: true}`) is the one-click toolchain install
Studio offers. Or build an image from
your toolchain's image
(`FROM golang:1.26-bookworm` with `RUN go install
github.com/go-delve/delve/cmd/dlv@latest`, or `FROM python:3.12-slim-bookworm`
with `RUN pip install debugpy`) and name it in `sandbox.image` (see
[the container tier's image](02-configuration.md#the-container-tiers-image)).
A session started without it ends at once with a problem naming what is
missing and these steps.

Delve is started with `--check-go-version=false`: a Delve release refuses, by
default, a program built with a Go newer than those it was tested with, and
the Go a session builds with can be newer than the Delve beside it. A program
built with a Go that Delve truly cannot read fails at launch with Delve's own
message.

## The bridge for Go

Delve does not speak DAP over standard input and output; it listens on a
socket. On a Mac the container runs in a VM, where a socket cannot be reached
from the host, so the engine runs Delve on a socket inside the sandbox and a
bridge beside it that carries the socket over the command's standard input
and output and does nothing else.

The bridge is a mode of Abhed itself (`abhed __abhed_dap_bridge
unix:PATH`), run from the Linux build of Abhed that the release archives and
Abhed Studio ship beside the binary for the sandbox's architecture, the same
helper that serves as the egress relay. A release build checks it against
the checksum it was built with, and the record says whether it was pinned.
It is mounted read-only into the sandbox for the session, at the path it has
on the host. Where the managed `relay_binary` is set, that build is used
instead. A Go launch without either is refused with the steps to get one.

## What is judged and recorded

You choose what to debug, not the adapter: the engine builds the adapter's
`launch` from the language, the program or test, its arguments, working
folder (inside the workspace) and environment you gave. That command line,
such as `dlv debug /work/cmd/app -- -v` or `python3 -m pytest tests/test_api.py`,
with any variables in front of it, is put to policy and the supervisor as
your `bash` call: a deny rule on it refuses the launch, and the supervisor
may refuse it too. A policy question is taken as answered, since you asked
for the launch; a question the supervisor raises is put to you in Studio as
a normal permission request, and the session starts only if you allow it.
It is recorded as your action, with its decision, and the session's end as
its observation.

While the session runs, Studio's messages pass to the adapter, except:

- a `launch` or `attach` of Studio's own, or a `restart` that changes what was
  launched, which Studio gets back as a failed response;
- anything but requests: the adapter's own requests are the engine's to answer;
- a message that repeats a key the engine decides on (`type`, `command`,
  `arguments` and the like) or has one equal to it under Unicode case
  folding without being it (`Command`, or `argumentſ`, which Go's JSON reader
  in Delve takes as `arguments`): programs differ on which of two such keys
  they read, so the engine refuses to guess. From the adapter such a message
  is dropped and counted. Output from the adapter the engine cannot read at
  all ends the session.

Studio's messages reach the adapter in the order Studio sent them.

The adapter's requests to run something in Studio's terminal
(`runInTerminal`, `startDebugging`) never reach Studio: they would start a
process on your machine, outside the sandbox. The engine answers them with
an error, and starts the program in the adapter's own console. Breakpoints,
stepping, variables and `evaluate` work as usual; `evaluate` runs code in the
program, which is in the sandbox.

A launch the adapter refuses ends with `status: failed` and the adapter's own
reason, such as Delve's "Go version … is too new", shown as text.

Each session is recorded as `debug.launched` (its id, the person's call id,
the language, the command line, the tier, and for Go the bridge and whether
its checksum was pinned) and `debug.stopped` (why it ended: `stopped`,
`ended`, `failed`, `adapter-missing`, or `closed` with its session; the
adapter's exit code, how long it ran, and how many of the adapter's messages
were dropped). Both are in the OCSF export as API activity
([OCSF events and logs](30-ocsf-and-logs.md)).

## Limits

- One connection holds at most four debug sessions; each ends after eight
  hours.
- One connection counts a launch still being judged against the four.
- A Go program built for debugging needs a writable build cache in the
  sandbox; pass `GOCACHE=/tmp/gocache` in the environment if the image's home
  folder is read-only.
- The container tier mounts `/tmp` `noexec`, so a debuggee built into `/tmp`
  cannot run there. Delve 1.27 builds the program in its working folder,
  which is fine; an older Delve, or a build you point at `/tmp`, fails at
  launch.
- Stopping a session asks the adapter to end the program, and ends the adapter
  if it has not gone within three seconds. Disconnecting from Studio's own
  debug toolbar, or terminating the program, ends the session the same way:
  once the adapter has answered, the engine closes its input and ends it after
  three seconds if it is still there.
