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

**Opening.** *Abhed: Reopen Folder in Dev Container…* needs a trusted
folder: its `devcontainer.json` chooses an image that runs as code. It reads
`.devcontainer/devcontainer.json` (or `.devcontainer.json`) and takes `image`
only. A `build` section or a Compose file is refused with the reason, and
lifecycle commands, `mounts`, `runArgs`, `containerEnv`, `remoteEnv`,
`forwardPorts` and `features` are listed as not applied. Studio asks in a
modal whose default is No, saying what will run and what it can reach. On a
yes the engine host process:

1. refuses an engine build whose real path is inside the folder;
2. checks the build against `abhed.devContainer.engineSha256` and copies it
   to a folder of Studio's own (`<user data>/abhed-devcontainer/engines/<sha256>/`,
   mode 700, the file 500), checking the copy too;
3. makes a fresh, empty volume `abhed-devc-<12 hex>-home` for the engine's
   home, removing an old one of that name;
4. runs the container runtime (`podman`, else `docker`, found on an absolute
   PATH entry) with an argv array, never a shell:

```
run -d --name abhed-devc-<12 hex of the folder's SHA-256> --init
    --label zybuu.abhed.devcontainer=1
    --security-opt no-new-privileges
    [--network none]                      abhed.devContainer.network: none
    -v <folder>:<folder>                  the folder, read-write, at the same path
    -v <Studio's copy>:/opt/abhed/bin/abhed:ro
    --mount type=volume,src=abhed-devc-<12 hex>-home,dst=/opt/abhed/home
    -w <folder> --entrypoint tail -- <image> -f /dev/null
```

5. refuses, and removes the container, when the image has `/etc/abhed`;
6. inspects the container and records the image it was made from (its id,
   and its registry digest when it has one), shown to the person.

**Running.** The window's choice is kept in Studio's workspace storage on
this computer, never in a file of the folder: a repository cannot point
Studio at a container. Before every start of the engine Studio re-hashes its
copy and inspects the container, refusing it when the image is not the one
recorded, when anything but Studio's copy is mounted at the engine's path or
that mount is writable, when the folder or the home volume is not where it
was put, when there is any other mount, or when Studio did not make it. Then
it runs `exec -i -w <folder> --env HOME=/opt/abhed/home --env
XDG_CONFIG_HOME=/opt/abhed/home/.config [--env ABHED_X= for each ABHED_*
the image sets] <name> /opt/abhed/bin/abhed acp`: the engine's home is
Studio's volume, never the image's `~/.abhed`, and the image's ABHED_*
variables are blanked. Only `acp` is passed. Nothing else changes for ACP.

**Leaving.** *Reopen Folder Locally* restarts the engine on the laptop and
keeps the container (and remembers its image, so a reopen still checks it).
*Remove Dev Container…* asks (default No) and removes the container and its
home volume, which deletes the record kept in it.

### Threat model

| Question | Answer |
|---|---|
| Who enforces policy? | The engine inside the container, with the configuration it reads: its own home on Studio's volume (empty until the person sets it, in an Abhed terminal), and the folder's `.abhed` under the same workspace trust. The image's `~/.abhed` is not read, its ABHED_* variables are blanked, and an image with `/etc/abhed` is refused. Studio enforces nothing new; it chose where the engine runs and what configuration it can read. |
| Which sandbox? | The engine's tiers, as available inside the container. The container is an outer boundary of its own, not a tier: an image without bubblewrap (or a container without user namespaces) gives the engine no process tier, and `sandbox.min_tier` (process by default) then refuses the session rather than fall to `none`; verified with a slim Debian image. Only the person can lower it, in the engine's configuration on Studio's volume. |
| What can the image do? | It is code chosen by whoever wrote `devcontainer.json`, and it runs with the folder mounted read-write: it can change any file in it, including `.git`, whose hooks and settings take effect when git runs on the laptop. It cannot change the engine (a read-only copy, re-hashed at every start) or its configuration home (Studio's volume; the image could still write there while it runs, since it shares the container, so a person who distrusts the image should not run it). |
| Where does the record live? | On the engine's home volume. It goes with *Remove Dev Container*. Shipping (`record.ship`) works from inside as anywhere. The laptop's record does not see the container's sessions. |
| How do credentials travel? | They do not, by default. No environment variable, SSH agent socket, home folder, `~/.abhed`, secret store, container-runtime socket or credential helper is passed or mounted. The engine reads model keys and secrets from its own store on the volume, which the person fills there (`abhed secret set` in an Abhed terminal). Forwarding is not offered in 1.3.0. |
| What does the container reach? | The folder (read-write), Studio's copy of the engine (read-only) and the home volume. With the default network it reaches the internet and the laptop's own services through the runtime's host gateway (`host.containers.internal`, `host.docker.internal`); `abhed.devContainer.network: none` gives it no network at all. The engine's egress rules apply to what the agent does, not to the image's own processes. |
| What still runs on the laptop? | The editor, its extensions, *New Host Terminal* and host-run tasks, which are on the host by design and say so, and the container runtime's CLI. Adding an MCP server or signing in (`abhed mcp add`, `abhed mcp login`) acts on the laptop's configuration, so in a dev-container window Studio does not offer them and says to run them in an Abhed terminal, which is inside. |
| Could a repository abuse this? | It can ship a `devcontainer.json` naming any image. Nothing happens until the person has trusted the folder, read the image in the modal and said yes. The image then runs as code with the folder writable and the network above, but without the laptop's credentials, and it cannot choose the engine, its configuration home or its managed policy. A tag that later names another image is refused at the next start. |

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
