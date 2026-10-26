# Toolchains: languages installed on demand

The sandbox's commands can build and run code in any of nine languages
without you preparing an image for them. When a command needs a language
toolchain that is not there, Abhed installs it, at the version you ask for
or the one the project pins, else the latest stable, and puts it first on the
commands' `PATH`. A developer working in several languages is never stopped
at "go: command not found".

| Language | `name` | Source (the project's own) | Checked against |
|---|---|---|---|
| Go | `go` | go.dev/dl | the release list's sha256 |
| Python | `python` | python-build-standalone (github.com/astral-sh/python-build-standalone), **not python.org**, which publishes no relocatable builds | the release's `SHA256SUMS` |
| Node.js | `node` | nodejs.org/dist | the release's `SHASUMS256.txt` |
| Rust | `rust` | the release channel on static.rust-lang.org (no rustup) | the channel manifest's sha256 |
| Java | `java` | Eclipse Temurin, from the Adoptium API | the API's sha256 |
| Maven | `maven` | Maven Central (repo.maven.apache.org) | its published sha512 |
| Gradle | `gradle` | services.gradle.org | its published sha256 |
| Ruby | `ruby` | ruby/ruby-builder (github.com/ruby/ruby-builder), the builds ruby/setup-ruby uses | digests **pinned in Abhed**, as the project publishes no checksum list; a version not in Abhed's table is refused |
| .NET SDK | `dotnet` | Microsoft's release metadata (builds.dotnet.microsoft.com) | its published sha512 |

## Where it comes from and where it goes

A download goes only to the language's own release hosts; a redirect to any
other host is refused. Its size and checksum are checked as it arrives, and a
download that does not match is thrown away. The archive is unpacked into
the per-user cache,

```
~/.cache/abhed/toolchains/<os>-<arch>/<language>/<version>
```

(under `XDG_CACHE_HOME` when that is set), refusing any entry or link that
would leave that folder and any device or special file, and then made
read-only. Nothing is installed system-wide, nothing runs with `sudo`, and no
installer script runs: Rust's components are laid out from their manifests,
and Ruby's scripts have the build machine's `ruby` path replaced with
`/usr/bin/env ruby`.

Commands read the cache and never write it. Under the process tier they get
the builds for this machine (`darwin-arm64`…); under the container tier the
cache is mounted read-only into each container and commands get the Linux
builds (`linux-arm64` on an Apple-silicon Mac with podman). The writable
caches builds need (`GOMODCACHE`, `CARGO_HOME`, npm's and pip's) stay the
sandbox's own, as before.

## Which version

1. The version asked for: "use Go 1.22", `go@1.22`, `python 3.11`.
2. Else the version the project pins, from the nearest folder at or above the
   command's that has one of `go.mod` (its `toolchain`, else its `go`
   line), `.python-version`, `.nvmrc`, `.node-version`,
   `rust-toolchain.toml` or `rust-toolchain`, `.java-version`,
   `.ruby-version`, `global.json` (its `sdk.version`) or `.tool-versions`.
3. Else the latest stable: the newest stable Go, the newest Python minor
   release python-build-standalone ships, the newest Node.js LTS, Rust's
   stable channel, the newest Temurin LTS, the newest final Maven and Gradle,
   the newest Ruby in Abhed's table, and the newest supported .NET channel's
   newest SDK.

A version is plain numbers, such as `1.22`, `3.11.4` or `21`; a prefix means
its newest release. Anything else in a pin file (a pyenv name, a Rust
nightly, a custom toolchain path) is left out: a pin is untrusted text. A Java
request names a feature version (`21`); a full version matches only when it
is that feature's newest.

Commands get, for each language, the newest installed version matching the
project's pin, else the newest installed. Its `bin` folder goes first on
`PATH`, with `JAVA_HOME`, `DOTNET_ROOT`, `MAVEN_HOME` or `GRADLE_HOME` where
the language needs one.

## How an install starts

- **The agent's `toolchain` tool.** When a command fails because `go`,
  `python3`, `node`, `cargo`, `java`, `mvn`, `gradle`, `ruby` or `dotnet`
  is not found, the command's result says which language Abhed can install
  and at which version, and the agent calls `toolchain` with `name`. Before
  you are asked, the call is resolved: its `name` becomes the language and
  major version (`go@1.22`), and `resolved` what will be installed, the
  version and why, the source, the URL, the size and the published checksum,
  whatever the model wrote there. The approval shows all of it. The install
  runs only if it still matches what you approved, and the agent is told to
  run its command again. **Nothing is installed or run again on its own**:
  a command whose earlier parts already ran is never repeated without its own
  call.
- **`abhed toolchain`.** `abhed toolchain install go python@3.12` from a
  terminal shows each install and asks; `-yes` installs without asking.
  `-platform linux` installs the builds a container's commands need.
  `abhed toolchain list` (`-json`) and `abhed toolchain remove go@1.22.5`.
  Inside an agent's command, install and remove are refused.
- **Abhed Studio**, which offers the install from its own modal, for instance
  when a Run or Test button's command exits 127
  ([ACP contract §3.2b](../architecture/studio-acp-contract.md)).

## Consent

```json
"sandbox": {
  "toolchains": "ask",
  "toolchains_preapproved": ["go", "python@3.12"],
  "toolchains_registries": "ask"
}
```

| Key | Default | |
|---|---|---|
| `toolchains` | `ask` | `ask`: the toolchain call is an ordinary ask, once per language and major version: its "always allow" is `toolchain(go@1.22)`. `auto`: installs without asking. `off`: nothing is installed, the tool is not offered, and a command missing a program is told installs are off. A workspace file may only make it stricter (auto, then ask, then off) |
| `toolchains_preapproved` | none | installs that need no ask: `go` (any version) or `python@3.12` (that major). Not from a workspace file until it is trusted |
| `toolchains_registries` | `ask` | see below. A workspace file may only set `off` |

Deny rules hold as always: `deny: ["toolchain"]` refuses every install, and
`deny: ["toolchain(rust@*)"]` refuses Rust. Where the managed file sets the
permissions, `auto` and `toolchains_preapproved` add no allow rules unless
the managed file sets them itself. The managed file can set `toolchains`
to `off` and lock it.

## Package registries

A toolchain is little use if its commands cannot fetch the project's
dependencies, and by default the sandbox has no network. `toolchains_registries`
is the **one way egress opens outside the managed file**, and only to each
language's official package registries:

| Language | Registries |
|---|---|
| Go | proxy.golang.org, sum.golang.org |
| Python | pypi.org, files.pythonhosted.org |
| Node.js | registry.npmjs.org |
| Rust | index.crates.io, static.crates.io, crates.io |
| Java, Maven | repo.maven.apache.org, repo1.maven.org |
| Gradle | plugins.gradle.org, services.gradle.org, downloads.gradle.org, repo.maven.apache.org |
| Ruby | rubygems.org, index.rubygems.org |
| .NET | api.nuget.org |

Where the network is off, commands go through the session's egress proxy
([Network policy](21-network-policy.md)), which refuses everything until you
agree to a language's toolchain; that agreement also opens the language's
registries, on port 443, and the approval names them. With `allow` every
registry is open from the start; with `off` none. The proxy fails closed:
only those hosts, only HTTPS, every connection recorded as an
`egress.decision`, and every opening as a `toolchain.registry`.

The proxy is used only where the chosen tier can hold it, and **never in
exchange for a weaker tier**. The process tier on macOS and Linux holds it; a
container tier whose engine is in a VM (podman or Docker Desktop on a Mac)
holds it with the Linux helper a release ships beside the binary as the
relay, or a Linux build named in `control_plane.<engine>.relay_binary`. Where it cannot be held the network
stays off, and the agent is told why when it tries.

With `allow_network: true` the network is open already and nothing changes.
Under a managed network (`sandbox.network`, `egress` or `allow_network` set
by the managed file), the registries join only where the managed file names
`sandbox.toolchains_registries` itself, and then they are added to the
managed allowlist, whose deny rules still win.

Why it exists: an agent that can install Go but not fetch a module stops at
the next command, and turning the whole network on to fix that would let
every command reach anywhere. Opening exactly the registries of the
languages you agreed to keeps the sandbox closed to everything else.

## Debuggers

Go and Python can come with their debugger, as part of the same agreement:
the offer reads "Go 1.27.3 + Delve debugger 1.27.2". Ask for it with the
toolchain tool's `debugger`, `abhed toolchain install -debugger go`, or
Studio's `_abhed/toolchain/install` with `debugger: true`, which its Run and
Debug offers when a debug session ends `adapter-missing`
([Debugging](31-debugging.md)).

| Language | Debugger | Where it comes from |
|---|---|---|
| Go 1.25 or later | Delve 1.27.2 (`dlv`) | built by Abhed itself, not in the sandbox: `go install github.com/go-delve/delve/cmd/dlv@v1.27.2` with the cached Go of the same version for this host (installed first, and offered, when the sandbox is Linux and this host is not), cross-built for the sandbox's Linux architecture with cgo off, from `proxy.golang.org` only, every module checked by `sum.golang.org`, with `GOFLAGS=-mod=mod`, `GOTOOLCHAIN=local` and none of your own Go settings (`GOPRIVATE`, `GOFLAGS`, `GOPROXY`…). Linux amd64 and arm64 only, where the debugger runs |
| Python 3.10 or later | debugpy 1.8.22 | its universal wheel from PyPI (`files.pythonhosted.org`), refused unless PyPI's listed sha256 is the one Abhed pins, unpacked where `PYTHONPATH` finds it |

Each is kept read-only in the cache beside the toolchains
(`<cache>/<platform>/extras/<name>/<version>`) and is on `PATH` (Delve) or
`PYTHONPATH` (debugpy) whenever its language is, for the agent's commands,
the terminal and the debugger. The tool's approval for a language and its
debugger is its own scope, `toolchain(go+debugger@1.27)`, so agreeing to Go
alone, or `toolchains_preapproved: ["go"]`, never agrees to building Delve;
`toolchains: auto` installs both. Each is recorded as its own
`toolchain.install` with `extra` (`delve`, `debugpy`), and Delve's with
`built_with` and the built binary's sha256.

## The record

Every install, by the agent or a person, is recorded as `toolchain.install`
with the language, version, platform, why that version, the source, URL,
published checksum, size, how it was agreed and how it ended; registries
opened are `toolchain.registry`. They are counted in
`abhed_toolchain_installs_total` and `abhed_toolchain_registries_opened_total`,
mapped to OCSF API Activity, and listed by [HawkEYE](15-hawkeye.md).
`abhed doctor` lists what the workspace needs, by its marker files and by
loose source files (a `.go` or `.py` file counts), and whether the cache has
it for the platform commands run on.

## What it does not do

- The workbench's interactive terminal gets the toolchains on `PATH` and a
  note naming the language for a missing program, through bash's
  `command_not_found_handle`; an image whose shell is not bash 4 or later
  gets `PATH` without the note.
- The microVM driver (`abhed-microvm`) gives its guest only block devices,
  the root filesystem and the workspace image, and shares no host folder, so
  the cache cannot be mounted there, read-only or otherwise, and commands
  there do not get the cache's toolchains on `PATH`. Put the toolchains a microVM needs in
  the root filesystem you build with `scripts/microvm/build-rootfs.sh`. The
  same holds for a server's per-session driver sandboxes, which run commands
  through their driver rather than the local sandbox.
- On a server, the registries are opened for the server's sandbox, which
  sessions sharing it share.
- Node.js downloads are checked against `SHASUMS256.txt` fetched over HTTPS
  from nodejs.org; its GPG signature is not checked.
- Ruby's builds link the system's libraries (OpenSSL, libyaml); the default
  `debian:bookworm-slim` image lacks libyaml, so `require "yaml"` fails there.
