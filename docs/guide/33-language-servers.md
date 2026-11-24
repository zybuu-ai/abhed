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

For Python, where `basedpyright-langserver` is not on the `PATH` commands
get but `pyright-langserver` or `pylsp` is, that one runs instead
(`pyright-langserver --stdio` first, then `pylsp`), so a Python server you
already have gives Studio its diagnostics, references and renames without
an install. The start you are asked about names the server chosen and what
it does: `pylsp` runs in the Python it was installed with, loads every
plugin installed beside it and its Jedi or Rope back end, and reads the
workspace's `setup.cfg`, `tox.ini`, `.flake8` and `pyproject.toml`. Under
the `container` tier this fallback never applies: the engine cannot look
inside the image, so `basedpyright-langserver` is always the one run there.

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

## Diagnostics after the agent's edits

The agent gets a language server's diagnostics for each file its `write` or
`edit` changed, in the CLI and in Studio alike, appended to the tool's
result:

```
Created a.go (23 bytes, 3 lines).

[gopls diagnostics for a.go: 1 errors, 0 warnings, 1 in all]
a.go:3:9: error: undefined: y (compiler)
```

This is the server above, run by the engine rather than an editor: one per
language per session, started on the first change to a file of that
language, in the session's sandbox, with the engine's settings and
`sandbox.language_servers` over the defaults. Its own requests are answered
by the engine: settings are the engine's, an edit it asks for is not
applied, nothing is opened. The engine sends the file's content as it is on
disk after the change and takes the server's next report for it.

| Setting | Default | |
|---|---|---|
| `tools.diagnostics` | `auto` | `auto` starts a server only where policy and the supervisor allow its command line outright, as a `bash` call (an allow rule such as `bash(gopls)`, or bypass mode); `on` also takes the setting as the answer to the plain ask a mode gives an unlisted command; `off` never |
| `tools.diagnostics_max` | 20 | diagnostics given per file, most severe first (at most 200); the rest are counted |
| `tools.diagnostics_wait_ms` | 3000 | how long an edit waits for them (at most 30000); a server not ready by then gives none for that edit |

Nobody is asked: under `auto` in the default mode the start is an ask, so
no server runs until an allow rule names it. Under `on`, only the plain ask
a mode or the default gives an unlisted command is answered, and the
PermissionRequest hooks (the managed file's too) hear it first and may
refuse it; a deny rule, a rule's ask, a destructive or screening ask and the
supervisor's word all hold. The supervisor reviews the start without
counting it against `monitor.max_per_turn`; when it cannot answer in time,
the start is refused for that edit only and the next edit asks again, three
times at most before diagnostics for the language are off for the session. The
start is recorded as the engine's call (`action.requested` with
`via: diagnostics`, then `action.approved` with `by: policy` or
`by: setting`, or `action.denied`), with its outcome as soon as the server
is up; the server's end is `lsp.stopped`. A start a hard kill left open is
closed when the session is next opened. The CLI shows it as a dim
`· diagnostics: language server gopls started …` note, not as a call of the
agent's; Studio shows a card titled `Language server: gopls` that completes
when the server is up. A start that was not allowed completes as
information, naming the allow rule (`bash(gopls)`) that would start it. It never enters the agent's conversation, and the
answer checker does not count it as a change. Where the server is not
installed, nothing is put to policy and diagnostics for the language are off
for the session, with one note (`lsp.diagnostics` with `skipped`, and in a
`-p` run one `abhed: note: no python diagnostics: basedpyright-langserver
is not installed where commands run; install it with: abhed toolchain
install -lsp python` line on stderr), never a permission denial. A start
that was not allowed is the same kind of note in `-p`, not an entry in
`permission_denials`. Diagnostics are off, with nothing recorded, where
commands have no sandbox, or the file is not Go, Python, TypeScript,
JavaScript or Rust. A
`rust-analyzer.toml` in the workspace keeps rust-analyzer from starting
(nobody is there to be asked), and one written while it runs stops it. A
server that fails to start, or ends on its own three times, is off for the
session. A workspace file, trusted or not, may only lower the bounds and
turn diagnostics off (a trusted one may also choose `auto` over `on`).

What the server reads is what the sandbox lets commands read: paths a `read`
deny rule names are not hidden from it, and a compiler message can quote a
value from such a file (a constant's text, a literal type), which then
reaches the agent. That is no wider than an allowed `bash(go build *)`, but
keep diagnostics `off`, or the server's command unallowed, where read rules
guard values the sandbox does not hide. Likewise, what the server writes in
the workspace (rust-analyzer's `cargo metadata` can update `Cargo.lock`) is
not kept by bash checkpoints, so `/undo` does not take it back. Sessions of
the server's web console get no diagnostics.

Recorded: `lsp.started` (with `allowed_by`, `step`, and the `rule` and
`layer` that allowed it) and `lsp.stopped`, both with `purpose: diagnostics` (not
counted in `abhed_lsp_starts_total`), and `lsp.diagnostics` per change with
the counts, `timed_out`, or `skipped` and why, counted in
`abhed_lsp_diagnostics_total` by language and outcome. Subagents do not get
diagnostics.

## Network

A server works with what the sandbox can reach. With the network off,
`gopls` and `rust-analyzer` see only the modules and crates already in the
workspace, vendored, or in the toolchain's cache; importing something new
shows as unresolved until it is fetched. Where the toolchain's package
registries were granted ([Toolchains](32-toolchains.md#package-registries)),
a server reaches them through the session's egress proxy like any command.
