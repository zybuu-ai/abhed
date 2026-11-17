# Studio remote: the engine in a dev container or on an SSH host

Status: 1.3.0. Both are built and tested end to end: the dev container with a
throwaway container (Studio's `devcontainer-smoke.mjs`), the SSH host with a
throwaway sshd container, its own key and known_hosts (`ssh-smoke.mjs`). See
[What is left](#what-is-left).

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

## SSH host (built)

**Transport.** The engine on the laptop connects, not OpenSSH: Studio's engine
host process runs the local, pinned engine as `abhed ssh-acp -host H -port P
-user U -sha256 HEX (-engine PATH | -upload FILE) [-identity F]
[-known-hosts F] -- FOLDER`, an argv of validated values. `ssh-acp` dials
with the engine's own SSH client (`internal/remote`), the one its `ssh` tool
uses:

- the host key must already be in known_hosts (`~/.ssh/known_hosts`, or
  `abhed.ssh.knownHostsFile`); an unknown key is refused, a different one is
  refused as possibly another machine, and nothing is ever added. The
  handshake asks for the key types known_hosts records for the host, as
  OpenSSH does, so a host recorded under one type is not refused for
  offering another first;
- it signs in with `abhed.ssh.identityFile` or the person's SSH agent, which
  is used to sign in and never forwarded. Nothing is forwarded: no agent, no
  X11, no ports, and no environment variable is sent;
- one remote command runs, through the account's shell: it checks the engine
  at the pinned path (`abhed.ssh.enginePath`) is a regular file, not a link,
  whose SHA-256 is `abhed.ssh.engineSha256`, refusing with exit 97
  otherwise; then `cd FOLDER` and `exec ENGINE acp`. ACP is carried on
  `ssh-acp`'s stdio, so Studio's ACP client is unchanged. With
  `abhed.ssh.uploadEngine` (a local build with the pinned hash) the build is
  first copied to `~/.abhed/bin/abhed-<16 hex>` on the host, under the
  engine's own state folder, checked there and put in place, then checked
  again at every start.

**Files.** The folder opens as `abhed-ssh://user@host:port/path`. Studio
registers a file system provider for that scheme whose every operation is
an ACP request to the engine on the host, in the window's session there
(`_abhed/fs/*`, studio-acp-contract.md):

- `stat`, `readDir`, `read`: inside the session's roots only; Abhed's own
  state and any path the read rules withhold are neither listed nor read;
- `write`: the person's save goes through the write tool on the manual path,
  so the write rules, the syntax check and the record apply (by: user); only
  text is written over it;
- `delete`, `rename`, `mkdir`: through the delete and write rules, refusals
  recorded;
- `search` and `files`: text search and quick open, walked by the engine
  over readable files (no `.git`, state or `node_modules`), Go regular
  expressions, bounded;
- `watch`: the engine polls the watched tree's sizes and times every 1.5 s
  and notifies `_abhed/fs/changed`; at most 64 watches and 20,000 entries
  each;
- `git`: the SCM view's read-only status and HEAD contents, run by the
  engine with hooks, fsmonitor, pager and external diff turned off. Studio
  shows the changed files and a diff with HEAD; staging and committing are
  left to the Abhed terminal.

Only an authority the person chose in *Reopen Folder on SSH Host* (kept in
Studio's application storage on this computer) is served or connected to,
so a workspace file or link naming another host connects nowhere.

### Threat model

| Question | Answer |
|---|---|
| Who enforces policy? | The engine on the host, with its configuration there (`~/.abhed`, `/etc/abhed`, the folder's `.abhed` under workspace trust). The person's file operations go through it too, so its read, write and delete rules hold for them. |
| Which sandbox? | The engine's tiers on the host. A host without a usable tier refuses a session below `sandbox.min_tier`, as anywhere. |
| Where does the record live? | On the host, under the engine's `~/.abhed`. The laptop's record does not see those sessions. |
| How do credentials travel? | The laptop signs in to the host with the person's key or agent; neither is forwarded, and no environment variable is sent. The engine on the host reads model keys and secrets from its own store there. |
| Who is the host? | Only one whose key is already in known_hosts; an unknown or changed key is refused before anything is sent. |
| Which engine runs? | Only a binary with the pinned SHA-256, checked on the host at every start (a check, then an exec, by the account's own shell: the account can change its own files between them). |
| What still runs on the laptop? | The editor, its extensions, the host terminal and host tasks. Language servers, completions, next edits, debugging, the Run and Review views and Studio's local git extension do not work on an SSH folder; the Abhed terminal and the agent run on the host. |

## What is left

- SSH host:
  - only text files are written (`write` refuses binary content);
  - the watch polls, so a change is seen within 1.5 s and very large trees
    (over 20,000 entries) are watched in part;
  - the SCM view is read-only (no stage, commit, branch or history), and
    language servers, completions, debugging and the Review view do not
    work on an SSH folder;
  - `~/.ssh/config` (ProxyJump, aliases) is not read; a jump host is not
    supported;
  - the check of the engine and its exec are two steps of the account's
    shell, not one file descriptor.
- Dev container: `build` (a Dockerfile), `features`, lifecycle commands and
  `remoteUser`; forwarding a model key on request, kept per container and
  recorded; routing `mcp add`/`mcp login` into the container
  (`exec --preserve-fds`).
- Paths the engine reports outside the folder (its record, an export written
  to its home) are container or host paths, which Studio cannot open on the
  laptop.
