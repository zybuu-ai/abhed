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

## What is checked against what

Every download is over HTTPS from the hosts below only (a redirect anywhere
else is refused), and its size and digest are checked as it arrives. For
most languages the digest comes from the **same origin as the archive**: it
catches a corrupted or truncated download and a tampered mirror or cache,
not a compromised project origin, which could change both. Only Ruby's,
Delve's, debugpy's and libyaml's are pinned in Abhed or checked by a
separate service.

| Language | Archive from | Digest from | Trust rests on |
|---|---|---|---|
| Go | dl.google.com (listed by go.dev/dl) | go.dev/dl's JSON release list, sha256 | go.dev's TLS; same origin |
| Python | github.com release assets of astral-sh/python-build-standalone (listed by api.github.com) | the same release's `SHA256SUMS` asset | GitHub's TLS and the project's release; same origin |
| Node.js | nodejs.org/dist | the release's `SHASUMS256.txt` (its GPG signature is not checked) | nodejs.org's TLS; same origin |
| Rust | static.rust-lang.org | the channel manifest there, sha256 | static.rust-lang.org's TLS; same origin |
| Java | github.com (Adoptium's releases) | api.adoptium.net, sha256 | Adoptium's API and GitHub, two hosts of one project |
| Maven | repo.maven.apache.org | the artifact's `.sha512` beside it | Maven Central's TLS; same origin |
| Gradle | services.gradle.org, downloads.gradle.org | services.gradle.org's checksum, sha256 | Gradle's TLS; same origin |
| .NET SDK | builds.dotnet.microsoft.com and Microsoft's CDNs | Microsoft's release metadata, sha512 | Microsoft's TLS; same origin |
| Ruby | github.com (ruby/ruby-builder releases) | **pinned in Abhed** per version and platform, as the project publishes no checksum list | the pin, which moves with Abhed's releases |
| libyaml (Linux Ruby) | deb.debian.org | **pinned in Abhed**, as packages.debian.org lists it | the pin |
| Delve | proxy.golang.org (every module) | **sum.golang.org**, the Go checksum database, a separate transparency log; the built binary's sha256 is recorded | the checksum database |
| debugpy | files.pythonhosted.org | **pinned in Abhed**, and PyPI's listing must agree | the pin |
| gopls | proxy.golang.org (every module) | **sum.golang.org**; the built binary's sha256 is recorded | the checksum database |
| basedpyright, typescript-language-server, typescript | registry.npmjs.org | the sha512 `integrity` **pinned in Abhed**, and the registry's listing must agree | the pin |
| rust-analyzer | github.com (rust-lang/rust-analyzer releases), unless the Rust release ships its own | the sha256 GitHub lists for each asset, **pinned in Abhed** (the project publishes no checksum file); a Rust release's own is checked with that release | the pin |

## Where it comes from and where it goes

A download goes only to the language's own release hosts; a redirect to any
other host is refused. Its size and checksum are checked as it arrives, and a
download that does not match is thrown away. The archive is unpacked into
the per-user cache,

```
~/.cache/abhed/toolchains/<os>-<arch>/<language>/<version>
```

(under `XDG_CACHE_HOME` when that is set), refusing any entry made through
a link, any hard link to anything but a file already unpacked, any link that
does not resolve to a path existing inside the toolchain or whose target
passes through another link, and any device or special file; Abhed's own
files in it are created only where nothing is, and it is then made
read-only. Nothing is installed system-wide, nothing runs with `sudo`, and no
installer script runs: Rust's components are laid out from their manifests,
and Ruby's scripts have the build machine's `ruby` path replaced with
`/usr/bin/env ruby`. Ruby's builds name the build machine's folders, so its
commands get `RUBYLIB` and `GEM_PATH` pointing at its own standard library
and gems, and on Linux a small `ruby` wrapper first on `PATH` that gives Ruby,
and only Ruby and what it starts, `LD_LIBRARY_PATH` to its own `libruby` and to
libyaml 0.2.5, which a Linux Ruby bundles from Debian 12's package
(`deb.debian.org`, its sha256 pinned in Abhed as packages.debian.org lists
it), since `debian:bookworm-slim` has none and YAML, RubyGems and Bundler
need it.

Commands read the cache and never write it. Under the process tier they get
the builds for this machine (`darwin-arm64`…); under the container tier the
cache is mounted read-only into each container and commands get the Linux
builds (`linux-arm64` on an Apple-silicon Mac with podman). The writable
caches builds need (`GOMODCACHE`, `CARGO_HOME`, npm's and pip's) stay the
sandbox's own, as before.

## Which version

1. The version asked for: "use Go 1.22", `go@1.22`, `python 3.11`.
2. Else the version the project pins, from the nearest folder at or above the
   command's, up to the workspace root and never above it, that has one of `go.mod` (its `toolchain`, else its `go`
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
| `toolchains` | `ask` | `ask`: the toolchain call is an ordinary ask, once per language and major version (the minor series for Go, Python, Ruby, .NET, Maven and Rust: `go@1.22`, `rust@1.82`; the major for Node, Java and Gradle: `node@22`): its "always allow" is `toolchain(go@1.22)`. `auto`: installs without asking. `off`: nothing is installed, the tool is not offered, and a command missing a program is told installs are off. A workspace file may only make it stricter (auto, then ask, then off) |
| `toolchains_preapproved` | none | installs that need no ask: `go` (any version) or `python@3.12` (that major). Not from a workspace file until it is trusted |
| `toolchains_registries` | `ask` | see below. A workspace file may only set `off` |

Deny rules hold as always: `deny: ["toolchain"]` refuses every install, and
`deny: ["toolchain(rust@*)"]` refuses Rust, with or without its debugger
(`toolchain(go@*)` also refuses `go+debugger@…`). Where the managed file sets
the permissions, `auto` adds no allow rule unless the managed file sets
`toolchains` itself, and `toolchains_preapproved` none unless it sets that
key itself. The managed file can set `toolchains` to `off` and lock it.

Permissions govern the agent's `toolchain` tool. A person's own install, at
`abhed toolchain install` or through Studio's offer, is their act and does
not consult allow, ask or deny rules; to stop people installing too, an
organisation sets `toolchains: off` in the managed file.

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
registries, on port 443, and the approval names them, as does Studio's
offer (`registries`). With `allow` every
registry is open from the start, recorded as a `toolchain.registry` with
`consent: allow` when each session starts; with `off` none. An opening lasts
for the sandbox's life: it is not closed again within it. The proxy fails closed:
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

## Language servers

Go, Python, Node.js (for TypeScript and JavaScript) and Rust can come with
their language server, as part of the same agreement, so Studio's editor
features ([Language servers](33-language-servers.md)) find it on `PATH`.
Ask for it with the toolchain tool's `lsp`, `abhed toolchain install -lsp
go`, or Studio's `_abhed/toolchain/install` with `lsp: true`, which it
offers when a server ends `server-missing`.

| Language | Server | Where it comes from |
|---|---|---|
| Go 1.26 or later | gopls 0.23.0 | built by Abhed itself, as Delve is: `go install golang.org/x/tools/gopls@v0.23.0` with the cached Go, from `proxy.golang.org` only, every module checked by `sum.golang.org`, none of your own Go settings; Linux and macOS on amd64 and arm64, and Linux on s390x |
| Python | basedpyright 1.40.2 (`basedpyright-langserver`) | its npm package, refused unless the registry's `integrity` is the sha512 Abhed pins. It runs on Node.js: where none is installed, Studio's offer and the CLI install Node.js first and say so; the agent's tool asks for Node.js on its own instead, under its own rules |
| Node.js 22.22.2 or later | typescript-language-server 6.0.2, with TypeScript 5.9.3 (the last with the `tsserver` it drives) | both npm packages, each refused unless its `integrity` is the pinned sha512 |
| Rust | rust-analyzer | the Rust release's own component where it ships one (Abhed's Rust installs do): it belongs to that Rust, in its folder, and goes when that Rust is removed, and the offer for a Rust not yet installed names that release as its source; else, for a Rust build without it, the 2026-10-12 release asset from GitHub, refused unless its sha256 is the pinned one |

They are kept read-only in the cache like the debuggers, recorded as their
own `toolchain.install` with `extra`, and are on `PATH` whenever their
language is. The approval is its own scope, `toolchain(go+lsp@1.27)` (with
the debugger too, `go+debugger+lsp@1.27`); deny and ask rules on the
language hold for it, and an allow for the language alone does not cover it.

## Debuggers

Go and Python can come with their debugger, as part of the same agreement:
the offer reads "Go 1.27.3 + Delve debugger 1.27.2". Ask for it with the
toolchain tool's `debugger`, `abhed toolchain install -debugger go`, or
Studio's `_abhed/toolchain/install` with `debugger: true`, which its Run and
Debug offers when a debug session ends `adapter-missing`
([Debugging](31-debugging.md)).

| Language | Debugger | Where it comes from |
|---|---|---|
| Go 1.25 or later | Delve 1.27.2 (`dlv`) | built by Abhed itself, not in the sandbox: `go install github.com/go-delve/delve/cmd/dlv@v1.27.2` with the cached Go of the same version for this host (installed first, and offered, when the sandbox is Linux and this host is not), built for the platform the session's commands run on (cross-built for a Linux sandbox) with cgo off, from `proxy.golang.org` only, every module checked by `sum.golang.org`, with `GOFLAGS=-mod=mod`, `GOTOOLCHAIN=local` and none of your own Go settings (`GOPRIVATE`, `GOFLAGS`, `GOPROXY`…). Linux and macOS on amd64 and arm64, where the debugger runs (macOS: the process tier, with the Xcode command line tools' debugserver) |
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

## Removing them

Installed toolchains are read-only, so a plain `rm -rf` of the cache fails.
`abhed toolchain remove go@1.22.5` removes one, and `abhed toolchain clean`
the whole cache, every toolchain and extra for every platform (asked first,
or with `-yes`).

Abhed Studio does the same through the engine: removing a language takes its
installed versions for the platform the session's commands run on; cleaning
asks you in Abhed's own prompt before removing the whole cache; and opening
registries opens an installed language's package registries to the
session's commands, showing the hosts first, as an install's offer does.
Each is your own act, refused inside an agent's command, and recorded as
`toolchain.install` (`outcome: removed`) or `toolchain.registry`.

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
- The fence tier's stdio MCP servers and a server's MCP servers in containers
  do not see read-only paths under your home, so they do not get the cache's
  toolchains; give such a server an image or a host that has its language.
- Every installed language is on `PATH` for every workspace's commands (the
  one a project pins first); a Linux Ruby's libraries reach only Ruby and
  what it starts, through its wrapper.
- Most digests come from the same origin as the archive (see "What is
  checked against what"); Node's `SHASUMS256.txt` GPG signature is not checked.
- A Linux Ruby links the image's OpenSSL 3 (`libssl.so.3`), which
  `debian:bookworm-slim` lacks, so there `require "openssl"` fails, and with
  it `gem install` and `bundle install` from rubygems.org; Abhed does not
  bundle OpenSSL, as a copy it pins would miss its security fixes. Use an
  image that has it (`sandbox.image`, for example `buildpack-deps:bookworm-curl`
  or your own `FROM debian:bookworm-slim` with `RUN apt-get install -y
  libssl3 ca-certificates`). `gem install` also writes to the build's own
  gem folder, which is read-only: install gems with Bundler into the
  workspace (`bundle config set path vendor/bundle`).
- Native gem extensions are built against the build machine's paths that
  `RbConfig` names, so gems with C extensions may not build.
