# Language servers for Abhed Studio

Abhed Studio's go to definition, hover, completion lists, references,
rename, diagnostics and the rest come from a language server, and the
engine runs it inside the session's sandbox, as a `bash` command would: the
same mounts, the same network rules, the same policy. Studio talks to it
through the engine over the connection; no port is opened on the host.

## Which server

`sandbox.language_servers` in your own or the managed configuration names
the command for a language, as for the workbench's editor
([The workbench](16-workbench.md#language-servers)); a workspace file cannot
name one. Where it names none, Abhed runs the server that comes with the
language's toolchain:

| Language | Server |
|---|---|
| `go` | `gopls` |
| `python` | `basedpyright-langserver --stdio` |
| `typescript`, `javascript` | `typescript-language-server --stdio` |
| `rust` | `rust-analyzer` |

The server must be on the sandbox's `PATH`. Abhed installs each of these with
its language ([Toolchains: Language servers](32-toolchains.md#language-servers)):
accept Studio's offer, or run `abhed toolchain install -lsp go` (`python`,
`node`, `rust`; add `-platform linux` for the container tier). When it is
not there, starting it fails with `server-missing`, a problem that names it
and gives that route first, then the sandbox image or
`sandbox.language_servers`; it also names the install itself
(`install: {language, lsp: true}`), so Studio's Install button installs
the language with its server in one step, after its usual consent modal.
When any toolchain or server is installed or removed for the session, by
Studio or by the agent's toolchain tool, the engine tells Studio
(`_abhed/toolchain/changed`), which starts the server again: no engine
restart. Where commands run on this machine's files (the
`process` and `fence` tiers), that is found before anything is judged: the
`PATH` commands get, with the installed toolchains' folders first, is where
the server is looked for, so a missing one is offered for install rather
than refused, and one the toolchain installed is not taken for an unknown
program. A start that policy or the supervisor refuses reaches Studio as a
`start-refused` problem with the reason and the steps, as a missing server
does.

It runs on every sandbox tier but `none`, the macOS process tier included:
unlike a debugger, a language server needs no loopback. Each runs with
`GOTOOLCHAIN=local GOFLAGS= GOENV=off`, so a `go.mod` cannot have it fetch
another Go and no flag such as `-toolexec` reaches its go commands from the
environment or a go env file.

## What a server may still run

A language server reads the workspace, and some read it by running
programs. Inside the sandbox, under its mounts, network rules and limits,
with the engine's settings:

| Server | Settings the engine sends | What it may still run |
|---|---|---|
| gopls | its defaults | `go list`, to load packages (and, for packages that use cgo, the cgo tool and the C compiler); no project code |
| rust-analyzer | build scripts, proc macros, check on save and cargo's autoreload off | `cargo metadata`, which reads the workspace's `.cargo/config.toml` and can run a `rustc` or `rustc-wrapper` it names; no `build.rs`, proc macro or `cargo check` unless a workspace `rust-analyzer.toml` you allowed turns them on (below) |
| typescript-language-server | `tsserver.path` set to the TypeScript installed beside the server | that TypeScript; never the workspace's `node_modules/typescript` (with none beside the server, starting it fails and says to install one) |
| basedpyright | its defaults | the Python interpreter it finds (the sandbox's `python3`, or one a `pyrightconfig.json` names) to read its import paths |

The engine sends these as the server's initialization options and as its
answers whenever the server asks for its settings, whatever Studio answers.
A command your operator names in `sandbox.language_servers` is the
operator's choice; the settings above still apply by language.

**The TypeScript beside the server.** Before typescript-language-server
starts, the engine looks for `tsserver.js` in two explicit places under the
server's own install, and nowhere else: Abhed's toolchain layout
(`<top>/bin/typescript-language-server`, with the packages in
`<top>/lib/node_modules`) and npm's global layout (the command links to the
package's `lib/cli.mjs`). In each it takes the package's own
`node_modules/typescript` or the `typescript` package beside it. There is
no module search, so neither the home folder's node folders, `NODE_PATH`
nor the workspace can supply it. One where a command could have put it is
refused: under a session folder, the sandbox's home or temporary folder
(`$HOME`, `$TMPDIR`, `/tmp`, `/var/tmp`), or the host's home folder. Abhed's
toolchain cache (`~/.cache/abhed/toolchains`, or under `XDG_CACHE_HOME`) is
the exception while the sandbox mounts it read-only, which it does on every
tier unless `sandbox.toolchains` is `off`: commands can only read it then, so
the TypeScript that `abhed toolchain install -lsp node` puts there is used.
With `sandbox.toolchains` off the cache is not exempt. If
none is found within 15 seconds, or the server ends first, it is stopped and
the start fails with `server-missing`.

**A workspace `rust-analyzer.toml`.** rust-analyzer reads
`rust-analyzer.toml` and `.rust-analyzer.toml` ahead of the editor's
settings, so one in a session folder (any folder under it, or any folder
above it) could turn build scripts and proc macros back on. When there is
one, starting rust-analyzer is put to you as a permission request that names
the files and says they override Abhed's settings: Allow once starts it,
Deny or no answer does not. The question and your answer are recorded with
the start (step `workspace`). When the supervisor has its own question about
the start, you get one question with both reasons, recorded together.

The check walks each session folder (skipping `target`, `node_modules` and
`.git`), counting links with the name too. If the walk stops at its bounds
(50,000 entries, or folders more than 12 levels deep), Abhed could not look
everywhere, and asks you as if it had found one, saying so.

The check is repeated while rust-analyzer runs. It is stopped (status
`ended`, reason `workspace-config-changed`, recorded) when:

- the agent or a subagent writes or edits a `rust-analyzer.toml`;
- Studio reports one opened, saved, changed, created or renamed;
- every 10 seconds, a `rust-analyzer.toml` appears, changes or goes in a
  session folder or a folder above it, or one found at the start changes.

Its next start asks you again. One written while you are still being asked
about the start stops it there: the server is not started. A file a command writes deeper in the tree,
which Studio does not report, is seen at the next start; until then the
running server may read it.

## What is judged and recorded

Starting a server is your `bash` call: its command line is put to policy and
the supervisor, and recorded with its decision; its end is the call's
observation. The engine then initializes the server itself, with the
workspace as its only folder, the editor's capabilities and the engine's own
initialization options (above; Studio's are never used, since they can set
a server's environment or tools), and hands Studio the server's
capabilities. A question the supervisor raises about the start is put to
you in Studio as a permission request, and the server starts only if you
allow it.

After that, what passes:

- **Studio's messages** go to the server only when they are standard LSP
  requests and notifications: a server's own methods, such as
  rust-analyzer's `experimental/runTest` or `rust-analyzer/runFlycheck`,
  which run tests, `cargo check` or build scripts, are refused. Also refused:
  an `initialize` of Studio's own, a settings change
  (`workspace/didChangeConfiguration`), a `workspace/executeCommand` whose
  command is not on the server's known-safe list, and anything naming a
  file outside the session's folders. A refused request
  gets an error answer, as from the server; each refusal is counted.
- **The server's messages** go to Studio except a request to open a page or a
  file outside the workspace (`window/showDocument`), which the engine
  answers itself with `success: false`. In notifications and answers, each
  item naming a file outside the workspace is dropped and the rest kept: a
  definition in the standard library leaves the workspace's results, a
  diagnostic's related information in a dependency leaves the diagnostic,
  and a related document outside leaves the report. A notification that is
  about an outside file as a whole is dropped; such an answer reaches
  Studio as `null`. A proposed edit is never trimmed: a rename, or a code
  action's edit, that touches any file outside the session's folders or in
  a `.abhed` folder is dropped whole (the rename's answer is `null`, the
  code action leaves the list), since an edit applied in part would leave
  the code broken. Edits the server proposes (`workspace/applyEdit`) go to Studio,
  which applies them to your buffers, unless one creates, renames, deletes
  or edits a file outside the session's folders or in a `.abhed` folder,
  which the engine refuses before Studio sees it; saving them is your
  write, as any save is. When the server asks for its settings, it is
  answered with the engine's whatever Studio replies. When Studio's answer
  to a request of the server's names a file outside, the server gets an
  error answer instead, so it does not wait.

A file counts as inside only when its URI is an absolute `file:` URI under
one of the session's folders, outside any `.abhed` folder, and still inside
with links followed: a bare or percent-encoded path, another scheme, or a
link in the workspace that leads out is outside. Only URI positions are
checked (`uri` and its kin, gopls's `URI` command arguments, the keys of
`changes` and `relatedDocuments`); a document whose text starts with `file:`
is just text. A link (a document link's `target`, a diagnostic's
`codeDescription.href`) may also be `http` or `https`; a `file:` link must be
inside, and any other scheme is outside.

Everything a server sends is treated as untrusted: it runs on the
workspace's code. Each message is read by its exact keys, and one with a key
written twice, or twice but for case (Unicode case folding included, as Go's
JSON reader matches keys: `ſ` is `s`, `K` is `k`), is dropped (from Studio, refused), so
the filter and the reader cannot see two different messages. Studio's
messages reach the server in the order Studio sent them.

A start and an end are recorded as `lsp.started` and `lsp.stopped`, the end
with how many of Studio's messages passed and how many were refused. Single
requests are not recorded: there are too many. The metrics count them
(`abhed_lsp_messages_total`), with the starts (`abhed_lsp_starts_total`).

## When a server crashes

A server that ends on its own is started again after a short wait (one
second, then two, four, eight and sixteen), up to five times, each start
judged and recorded again. Studio is told with `status: restarted` and the
server's capabilities, and opens its documents again. Until then Studio's
messages are refused as "restarting" rather than lost. After the fifth, or
when a restart is refused, Studio gets `status: ended`. Closing the session
records `closed`.

## Network

A server works with what the sandbox can reach. With the network off,
`gopls` and `rust-analyzer` see only the modules and crates already in the
workspace, vendored, or in the toolchain's cache; importing something new
shows as unresolved until it is fetched. Where the toolchain's package
registries were granted ([Toolchains](32-toolchains.md#package-registries)),
a server reaches them through the session's egress proxy like any command.
