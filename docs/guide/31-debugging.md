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
- **The Linux process tier**, where bubblewrap gives each command its own
  network namespace.

Not the macOS process tier: its loopback is the host's, and opening it would
let the sandboxed program reach the services on your machine. A launch there
is refused with a problem that says to use the container tier: start a
container engine (`podman machine start`), set `"sandbox": {"min_tier":
"container"}`, and open a new session.

The debugger must be in the sandbox image: Delve for Go, `debugpy` for
Python. The default image has neither; build one from your toolchain's image
(`FROM golang:1.26-bookworm` with `RUN go install
github.com/go-delve/delve/cmd/dlv@latest`, or `FROM python:3.12-slim-bookworm`
with `RUN pip install debugpy`) and name it in `sandbox.image` (see
[the container tier's image](02-configuration.md#the-container-tiers-image)).
A session started without it ends at once with a problem naming what is
missing and these steps.

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
may refuse it too. It is recorded as your action, with its decision, and the
session's end as its observation.

While the session runs, Studio's messages pass to the adapter, except:

- a `launch` or `attach` of Studio's own, or a `restart` that changes what was
  launched, which Studio gets back as a failed response;
- anything but requests: the adapter's own requests are the engine's to answer.

The adapter's requests to run something in Studio's terminal
(`runInTerminal`, `startDebugging`) never reach Studio: they would start a
process on your machine, outside the sandbox. The engine answers them with
an error, and starts the program in the adapter's own console. Breakpoints,
stepping, variables and `evaluate` work as usual; `evaluate` runs code in the
program, which is in the sandbox.

Each session is recorded as `debug.launched` (its id, the person's call id,
the language, the command line, the tier, and for Go the bridge and whether
its checksum was pinned) and `debug.stopped` (why it ended, the adapter's exit
code and how long it ran). Both are in the OCSF export as API activity
([OCSF events and logs](30-ocsf-and-logs.md)).

## Limits

- One connection holds at most four debug sessions; each ends after eight
  hours.
- A Go program built for debugging needs a writable build cache in the
  sandbox; pass `GOCACHE=/tmp/gocache` in the environment if the image's home
  folder is read-only.
- Stopping a session asks the adapter to end the program, and ends the adapter
  if it has not gone within three seconds.
