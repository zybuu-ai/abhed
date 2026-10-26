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

The server must be on the sandbox's `PATH`. When it is not, starting it fails
with a problem that names it and says how to install it, with the language's
toolchain, or in the sandbox image.

It runs on every sandbox tier but `none`, the macOS process tier included:
unlike a debugger, a language server needs no loopback. Each runs with
`GOTOOLCHAIN=local`, so a `go.mod` cannot have it fetch another Go.

## What is judged and recorded

Starting a server is your `bash` call: its command line is put to policy and
the supervisor, and recorded with its decision; its end is the call's
observation. The engine then initializes the server itself, with the
workspace as its only folder, the editor's capabilities and no
initialization options (which can set a server's environment or tools), and
hands Studio the server's capabilities.

After that, what passes:

- **Studio's messages** go to the server except an `initialize` of its own,
  a settings change (`workspace/didChangeConfiguration`; the server keeps its
  defaults), a `workspace/executeCommand` whose command is not on the
  server's known-safe list (a command can have a server run programs), and
  anything naming a file outside the session's folders. A refused request
  gets an error answer, as from the server; each refusal is counted.
- **The server's messages** go to Studio except a request to open a page or a
  file outside the workspace (`window/showDocument`), which the engine
  answers itself with `success: false`; notifications about files outside the
  workspace, which are dropped; and answers naming them, which reach Studio
  as `null`. Edits the server proposes (`workspace/applyEdit`) go to Studio,
  which applies them to your buffers; saving them is your write, as any save
  is. When the server asks for its settings, it is answered with its
  defaults whatever Studio replies.

Everything a server sends is treated as untrusted: it runs on the
workspace's code. Each message is read by its exact keys, and one with a key
written twice, or twice but for case, is dropped (from Studio, refused), so
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
server's capabilities, and opens its documents again. After the fifth, or
when a restart is refused, Studio gets `status: ended`.

## Network

A server works with what the sandbox can reach. With the network off,
`gopls` and `rust-analyzer` see only the modules and crates already in the
workspace, vendored, or in the toolchain's cache; importing something new
shows as unresolved until it is fetched. Where the toolchain's package
registries were granted ([Toolchains](32-toolchains.md#package-registries)),
a server reaches them through the session's egress proxy like any command.
