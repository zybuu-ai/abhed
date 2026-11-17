# Studio remote: the engine in a dev container or on an SSH host

Status: 1.3.0. The dev-container path is built and tested end to end with a
throwaway container (Studio's `devcontainer-smoke.mjs`); the SSH path is
designed here and not built (see [What is left](#what-is-left)).

## The problem

Studio drives one engine per window, `abhed acp`, as a child of its engine
host process on the person's computer. Work that belongs on another machine,
a project's dev container or a build host reached over SSH, then runs its
commands, tools and language servers on the laptop, under the laptop's
sandbox. The goal is the opposite: the editor stays on the laptop, and the
engine, with its policy, sandbox and record, runs where the work is.

## What Code-OSS offers, and why it is not used

Code-OSS ships the remote server (`src/vs/server`, the "remote extension
host") and the resolver API it is reached through. The resolvers that make it
useful, for SSH and for dev containers, are not part of Code-OSS: they are
separate extensions under their own licences and are not in the fork. Using
the remote server would also mean installing a matching Studio server build on
every target and running the editor's extension host there, which is a second
place that runs code and reaches the network, outside the engine's policy.

Studio needs less. Everything the agent does already goes through one stdio
stream, ACP, to one process that enforces the policy. So the transport is
moved, not the editor: the engine host process starts `abhed acp` inside the
target instead of beside itself, and ACP flows over that process's stdio as
before. The editor, its extensions and its files stay local.

## Dev container (built)

**Opening.** *Abhed: Reopen Folder in Dev Container…* reads the folder's
`.devcontainer/devcontainer.json` (or `.devcontainer.json`). The smallest
version takes `image` only. A `build` section is refused with the reason,
and `initializeCommand`, `postCreateCommand`, `mounts`, `runArgs`,
`containerEnv`, `remoteEnv`, `forwardPorts` and `features` are listed as not
applied. Studio asks in a modal whose default is No, saying exactly what
will run. On a yes the engine host process runs the container runtime
(`podman`, else `docker`, found on an absolute PATH entry) with an argv
array, never a shell:

```
run -d --name abhed-devc-<12 hex of the folder's SHA-256> --init
    --label zybuu.abhed.devcontainer=1
    --security-opt no-new-privileges
    -v <folder>:<folder>                  the folder, read-write, at the same path
    -v <linux engine>:/opt/abhed/bin/abhed:ro
    -w <folder> --entrypoint tail <image> -f /dev/null
```

The folder is mounted at the same absolute path, so the paths the editor and
the engine use agree and no path is translated anywhere. The engine is a
Linux build the person chose in `abhed.devContainer.enginePath` (a user
setting, never a workspace one), pinned by `abhed.devContainer.engineSha256`
and checked before every mount and every start. An existing container of
that name is started, not recreated.

**Running.** The window's choice is kept in Studio's workspace storage on
this computer, never in a file of the folder: a repository cannot point
Studio at a container. With it set, the engine host process starts the
engine as `exec -i -w <folder> <name> /opt/abhed/bin/abhed acp`. Only `acp`
is passed, as locally. Nothing else changes for ACP.

**Leaving.** *Reopen Folder Locally* restarts the engine on the laptop and
keeps the container. *Remove Dev Container…* asks (default No) and runs
`rm -f -v` on it, which deletes the record kept inside it.

### Threat model

| Question | Answer |
|---|---|
| Who enforces policy? | The engine inside the container, with the configuration it reads there: its own `~/.abhed`, the folder's `.abhed` under the same workspace trust, and the container's `/etc/abhed` if the image has one. Studio enforces nothing new; it only chose where the engine runs. |
| Which sandbox? | The engine's tiers, as available inside the container. The container is an outer boundary of its own, not a tier: an image without bubblewrap (or a container without user namespaces) gives the engine no process tier, and `sandbox.min_tier` (process by default) then refuses the session rather than fall to `none`; verified with a slim Debian image. The person may set `sandbox.min_tier: "none"` in the engine's own configuration inside, a choice made there and recorded there. The policy is the same whatever the tier. |
| Where does the record live? | In the container, under the engine's `~/.abhed`. It goes with the container: *Remove Dev Container* deletes it. Shipping (`record.ship`) works from inside as anywhere. The laptop's record does not see the container's sessions. |
| How do credentials travel? | They do not, by default. No environment variable, SSH agent socket, home folder, `~/.abhed`, secret store, container-runtime socket or credential helper is passed or mounted. The engine reads model keys and secrets from its own store inside the container, which the person fills there (`abhed secret set` in an Abhed terminal). Forwarding is not offered in 1.3.0. |
| What does the container reach? | The folder (read-write) and the engine binary (read-only). Its network is the runtime's default; the engine's own egress rules apply to what the agent does. |
| What still runs on the laptop? | The editor, its extensions, *New Host Terminal* and host-run tasks, which are on the host by design and say so, and the container runtime's CLI. Adding an MCP server or signing in (`abhed mcp add`, `abhed mcp login`) acts on the laptop's configuration, so in a dev-container window Studio refuses them and says to run them in an Abhed terminal, which is inside. |
| Could a repository abuse this? | It can ship a `devcontainer.json` naming any image. Nothing happens until the person reads the image in the modal and says yes; the image then runs with the folder only, not the laptop's credentials. A repository cannot set the engine binary, its pin, or the window's choice. |

## SSH host (designed, not built)

The same transport over SSH: `ssh -T -o BatchMode=yes -o ForwardAgent=no
-o ClearAllForwardings=yes -o PermitLocalCommand=no <host> -- abhed acp`,
with the engine installed on the host and the folder a path there. Policy,
sandbox and record are the host's, as for the container. Credentials are
not forwarded: no agent, no X11, no ports, no `SendEnv`.

What makes it larger than the container: the folder lives on the host, so
the editor needs the host's files. Without the Code-OSS remote server that
is a file system provider over the same connection (a second ACP-like
channel, or the engine serving read and write of the workspace to Studio
under its policy), and search, the SCM view and file watching over it. That
is the part not built.

## What is left

- SSH host: the file system provider and its watcher; host-key handling
  (Studio's own known_hosts check before connecting).
- Dev container: `build` (a Dockerfile), `features`, lifecycle commands and
  `remoteUser`; a named volume for the engine's `~/.abhed`, so the record
  outlives the container; forwarding a model key on request, kept per
  container and recorded; routing `mcp add`/`mcp login` into the container
  (`exec --preserve-fds`).
- Paths the engine reports outside the folder (its record, an export written
  to its home) are container paths, which Studio cannot open on the laptop.
