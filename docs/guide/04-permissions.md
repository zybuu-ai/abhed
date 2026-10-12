# Permissions and safety

An agent that can run shell commands in your repository needs a real answer to
"what is it allowed to do". Abhed's answer is a policy engine that decides every
tool call, in a fixed order, with one rule that nothing can override.

## Modes

```json
"permissions": { "mode": "default" }
```

| Mode | Behaviour |
|---|---|
| `default` | ask before every mutation |
| `plan` | **read-only** — nothing is written, safe for exploring an unfamiliar repository. A command or edit is refused as plan mode before any destructive or ask step, so it is never put to you |
| `accept-edits` | auto-approve file edits, still ask for shell |
| `auto` | approve by rule; anything unmatched still asks |
| `bypass` | approve everything an org policy has not forbidden. Dangerous, and refusable by managed settings |

In headless mode there is no one to ask, so anything needing approval is
refused. Use `-mode auto` with explicit `-allow` rules.

### Changing mode in the CLI

`/mode <name>` changes the mode; `/mode` on its own shows the mode in force,
the Shift-Tab cycle, the turn limit and, in auto mode, what auto approves.

- **Shift-Tab** cycles `default` → `accept-edits` → `plan` → `default`. It
  never reaches `auto` or `bypass`, however often it is pressed. A managed
  `cli.mode_cycle` can take modes out of the cycle and never add one; a
  managed `permissions.mode` leaves only that mode and `plan`.
- **`/mode auto`** asks first, with no as the default: Enter, a no, or input
  ending leaves the mode as it was. Over a managed `permissions.mode` it is
  refused before you are asked.
- **`bypass`** is chosen only at startup, with `-mode bypass`, and is refused
  under a managed configuration. Deny rules still refuse in bypass.

Every change is recorded as `mode.changed`, with the mode it came from, the
mode it went to and how: `flag`, `slash`, `shift-tab` or `plan-exit`. A mode
chosen before the first message is recorded when the conversation opens,
ahead of that message. A continued session (`-c`, `-r`) that starts in
another mode than its record ended in records the change as `flag` when
`-mode` chose it, `carried` when it was changed before the first message, and
`config` when it is simply the configured mode.

**Auto mode is rules, not a judgment.** It approves read-only tools, and
`edit` and `write` inside the workspace, without asking; a command (`bash`)
runs without asking only where an allow rule covers it. It still asks for
anything an ask rule names, for destructive commands, and for reads that can
send data out, and deny rules still refuse. Every approval and denial is recorded
with the step that decided and, where a rule did, the rule (`step`, `rule`,
`reason`); `/permissions explain` shows the same for a call you name.

### Plan mode

In `plan` mode nothing is changed, and the agent has one extra tool,
`exit_plan`, which it calls with its plan when it is ready. In every other
mode the tool is not offered, and a call to it is refused as an unknown tool.
Calling it changes nothing, the mode included: the plan is recorded as
`plan.proposed`, the run ends, and the CLI shows the plan and asks:

```
  Proceed with this plan?
  │ 1. Create notes.txt …
  1. Yes, and accept edits
  2. Yes, ask before each change
  3. No, keep planning (tell it what to change)
  answer 1-3, or enter for No, keep planning (tell it what to change):
```

Keeping planning is the default. Auto and bypass are never offered, and a
mode the managed configuration would refuse is left out. The answer is
recorded as `plan.decided`; accepting changes the mode through the same path
as `/mode`, recorded as `mode.changed` with `via: plan-exit`, and the
conversation goes on with "The plan is approved (mode …). Carry it out." as
the next message. To keep planning, say what to change.

### Session rules: `/permissions`

`/permissions` lists every rule in force with where it came from:
`managed (locked)`, `user`, `workspace` (or `workspace (untrusted: tightens
only)`), `flag`, `default`, and `session` for the rules added below. Settings
an untrusted workspace file made that were ignored are named under it, and so
are allow rules from `~/.abhed/config.json` or a workspace's
`.abhed/config.json` left out because the managed configuration sets the
permissions without its own `permissions.allow`.

`/permissions allow|ask|deny <rule>` adds a rule for this session only:

- A deny or ask rule only tightens, so it is added as given.
- An allow rule is asked about first, with no as the default. One that
  approves every call to a tool (`bash`, `bash(*)`, `write(**)`, `*`) is
  asked about twice.
- An allow rule is refused when the managed configuration sets any
  `permissions` setting. A "Yes, and don't ask again" answer to a prompt is
  not a rule: it covers only the scope the prompt names (for `bash`, one of
  the short list below; for a file, that one path; for `web_fetch`, the site;
  for `ssh`, the host and command, as `host/command`; for another tool,
  that call's subject, such as an MCP tool's `path`, `url` or `command`,
  or every call to the tool when the call has none) for the
  rest of the session, and
  still applies under a managed configuration.
- Session rules are evaluated after the configured ones in each list, so a
  session allow approves only what would otherwise ask: it cannot lift a deny
  rule, a destructive command, an ask rule or plan mode. It does not allow a
  `secret`, which needs a configured rule.
- Each change is recorded as `permission.changed` with `scope: session`.
  `/permissions remove <rule>` takes one out again.
- `/clear` and `/resume` end the session's rules.

`/permissions explain <tool> <command, path or url>` (the tool name in any
case) is a dry run: it prints
the decision, the step, the rule and the reason policy would give, for
example `deny · step deny · bash(curl *) · denied by rule bash(curl *)`.
Hooks are not asked, since that would show them a call that is not being made.
A `read`, `write` or `edit` the tool itself would refuse, a path outside the
reachable folders or one kept from the agent such as Abhed's own state, shows
as `refused · by the <tool> tool`, with what policy alone would have said.

### Adding a directory: `/add-dir`

`/add-dir <dir>` lets the session reach another directory. It is bound as the
`-add-dir` flag is: a managed `additional_dirs` refuses it. The directory is
shown with its links resolved, and you choose read only, read and write, or
no, which is the default. Read-only is held by deny rules on `edit` and
`write` for that directory, which `/clear` keeps, so `accept-edits` does not
approve changes there unless you granted write; commands run by `bash` are
asked about as they are anywhere. Abhed's own state (`~/.abhed`, a
workspace's `.abhed`, the record directory, a configured state file),
credential folders such as `~/.ssh` and `~/.aws`, and any folder that holds
your home directory are refused, judged by where a link leads. The folder
added is the one checked and shown: if the path leads somewhere else by the
time you answer, nothing is added. The `-add-dir` flag and `additional_dirs`
refuse the same folders, except that a folder inside `~/.abhed` that holds
none of its state, such as a skill's, may be added there. A folder that holds
the workspace, such as a monorepo's root, may be added either way; the
workspace's `.abhed` stays out of reach through it. Each added
directory is recorded as `workspace.dir_added` with its access.

### Turn limit

`limits.max_turns` bounds how many turns the agent takes. In the interactive
CLI it applies to each message you send, so a long conversation does not run
out for good: a message that reaches it stops with "send "continue" to let it
go on". Where the managed configuration sets `limits.max_turns`, it bounds
the whole conversation instead, as the organisation wrote it, and `/clear`
starts a new one. `/mode` says which applies. Headless runs and the server
count the whole conversation.

## Rules

A rule is a tool name, optionally followed by a pattern:

```json
"permissions": {
  "mode": "auto",
  "allow": ["bash(go test*)", "bash(npm run *)", "read"],
  "deny":  ["bash(curl *)", "write(*.pem)"],
  "ask":   ["bash(git push*)"]
}
```

The built-in ask rules include the read tool on files that commonly hold
credentials, in the workspace or below it: `.env` and `.env.*`, `*.pem`,
`*.key`, and SSH keys named `id_rsa*`, `id_ed25519*` or `id_ecdsa*`. This is
a question before the read tool opens one, **not credential protection**: a
command such as `cat .env` (allowed by the built-in `bash(cat *)`), a search,
or a program the agent runs can still read the file. Keep credentials out of
the workspace, give a secret only to the command that needs it
([Secrets](#secrets)), and deny reads of such files and the commands that
print them in your own or the managed rules. Your own `permissions.ask` list
replaces the built-in one, so copy these rules into it if you set one.

The tool name of a built-in tool, or of an action such as `secret`, `delete`
or `forge_pr`, may be written in any case: `Bash(curl *)`, `BASH(curl *)` and
`bash(curl *)` are the same rule, as are `Read(**/.env)` and `read(**/.env)`,
so a rule copied from another tool's settings holds. This applies to allow
rules too, so `Read` allows `read`, as it was evidently meant to. Only the
tool name is folded: the pattern keeps its case, and a rule never moves to
another tool. An extension may not provide a tool named like a built-in in any
case; such a tool is refused with a warning. MCP tool names
(`mcp__server__tool`), knowledge-base tools (`rag_name`) and extension tools
are compared exactly, case included, because their own protocols and
configuration tell `GitHub` from `github`.

`mcp__<server>__*` as the tool name names every tool of that one MCP server,
as it does in an agent definition's tool list: `"deny": ["mcp__github__*"]`
refuses every tool the `github` server offers, and may take a pattern too,
`mcp__github__*(repo/*)`. The server name is compared exactly. It is the only
wildcard in a tool name; any other `*`, `?` or space in an MCP tool name, or
an MCP rule with no tool part (`mcp__github`), never matches and is reported
as below. Server and tool names may themselves hold `__`, so
`mcp__github__ent__x` could be server `github`'s tool `ent__x` or server
`github__ent`'s tool `x` (and `mcp__a___x` both `a`'s `_x` and `a_`'s `x`).
Abhed does not start a server whose name overlaps another's this way (here
`github__ent`; added at run time, whichever of the two comes second), and
leaves out any server's tool whose full name begins with another configured
or refused server's `mcp__<name>__`; each is a warning, and `abhed doctor`
fails on such a pair. Rename one of the two servers.
An allow wildcard that could reach past its server either way, such as
`mcp__github__*` beside a `github__ent` server or `mcp__github__ent__*`
beside a `github` server, is listed too. Spaces around a tool name are ignored: `bash (curl *)` is
`bash(curl *)`.

A rule whose tool name names no tool is not silent. Abhed warns of it at
startup, and `abhed doctor` (and `/doctor`, and Abhed Studio's checks) lists
it, with the name it was probably meant to be (`WebFetch` → `web_fetch`). This
covers a name that is no built-in; an MCP rule for a server that is not
configured, or for a configured one written in another case (`mcp__GitHub__x`
when the server is `github`); a `rag_` name no knowledge base here has; and,
once the session's tools are known, an MCP or extension tool written in
another case. A rule from your own files is a warning only.

In the managed configuration, a deny or ask rule listed so fails `abhed
doctor`; an allow rule listed so is a warning, since it only allows less.
Abhed still starts, because a server, knowledge base or extension tool the
rule names may be set up later or on another machine. Only a managed deny
rule whose tool name is evidently a mistake stops Abhed from starting, with a
message naming the rule and the file, since Abhed should not run while the
organisation believes such a rule holds. Evidently a mistake means: another
tool's name for a built-in (`Shell`, `WebFetch`, `MultiEdit`), or `MCP__`
written in another case, while the managed configuration sets no extension
that may provide tools (with one, it may be that extension's tool, so it
fails the doctor instead; an extension from your own files does not count); a
built-in written with look-alike letters (the Kelvin sign for `k`); or an MCP server or knowledge base that the managed
configuration itself sets, written in another case. A name merely close to a
built-in, such as `git`, `sql` or `bsh`, may be a tool set up later: it fails
`abhed doctor`, with the built-in it may mean, but does not stop startup. A bash pattern ending in
`:*`, another tool's way of writing a prefix, is read literally (a colon, then
anything) and is listed too: write `bash(npm test *)` for `Bash(npm test:*)`.
The `-allowedTools` and `-disallowedTools` flags still translate `:*` for you. `-disallowedTools`
denies an MCP or extension tool both as written and in lower case, as it
always lowered names; `-allowedTools` allows it only as written.

`*` matches anything, newlines included; the pattern is matched against the
command or path, or for a tool with neither, the first of its `pattern`,
`action`, `resource`, `host`, `namespace`, `name` or `url` arguments. So an
MCP tool that takes only a `url` is matched on it:
`mcp__browser__open(https://intranet.example/*)`. `ssh` is matched as
`host/command` (`ssh(build1/uptime)`, `ssh(*/uptime)`), so an answer for one
host never covers another; deny and ask rules also read the command alone.
A subject given as a list of strings is read as its words joined by blanks;
any other kind is refused while a deny or ask rule with a pattern applies.

For `web_fetch` the pattern is matched against the URL exactly as the model
wrote it, and the tool fetches that string or nothing. It only fetches a URL
written in one form — no spaces around it, lower-case scheme and host, no user
name, no trailing dot on the host, a port written as a plain number from 1 to
65535 and left out when it is the scheme's default, an address written as
four decimal numbers (IPv4) or in compressed form (IPv6, never an IPv4 address
written as IPv6), a path of at least `/` with no `.`, `..` or empty segment
(raw or `%`-encoded), no needless `%` escapes and no `#fragment` — and
refuses any other spelling. A host whose last part is a number and is not an
IPv4 address, such as `1572395042` or `127.1`, is refused, since a resolver
reads it as an address. So `"deny": ["web_fetch(https://example.com/*)"]`
cannot be stepped around by writing `HTTPS://Example.COM:0443`, and
`web_fetch(https://example.com/admin*)` cannot by writing `/public/../admin`.

Two things are not normalised. A path rule matches case-sensitively:
`web_fetch(http*://example.com/admin*)` does not match `/Admin/`, which a
server that ignores case (IIS, or a static server on a case-insensitive disk)
serves as `/admin/`. A query is matched exactly as written, its parameters'
order and duplicates included. For a server that ignores case, or content you
must keep out whatever the path or query, deny the host:
`web_fetch(http*://example.com/*)`.

Write a deny rule for a host so it covers both schemes:
`web_fetch(http*://example.com/*)`. A rule written with `https://` alone
leaves `http://` to the same host open. A host on another port needs its own
rule, such as `web_fetch(http*://example.com:8080/*)`. A redirect to anything
but the same URL is not followed but handed back, so it is judged as a call
of its own.

A deny or ask path pattern matches the path as the tool was given it, the
absolute path, the path with its links resolved, and the path relative to the
workspace and to each added directory, with or without a leading `./`. So
`write(docs/**/frozen/**)`, `write(./docs/**/frozen/**)`,
`write(**/frozen/**)` and `write(/abs/path/to/ws/docs/**)` all refuse a write
to `docs/guide/frozen/a.md`, however the agent or the Explorer spells it.
Relative patterns never match a path outside every workspace root.

An allow path pattern matches only where the call lands: the path with `..`
and links resolved, absolute or relative to the workspace. `write(notes/**)`
does not allow a write through a link in `notes/` to somewhere else, nor
`notes/` in an added directory, nor `notes/../src/x`. Write an absolute
pattern for an added directory.

A deny or ask path pattern is also matched against the path as the disk
spells it. On a disk that ignores case (macOS and Windows by default),
`DOCS/Frozen/f.md` opens `docs/frozen/f.md`, so `write(docs/frozen/**)`
refuses it, in any mix of cases, from the agent's tools and the Explorer
alike. Each folder and file that exists is looked up under the name the disk
holds; a name that does not exist yet, such as a new file or folder, keeps
its spelling, under its folder's real name. Allow patterns still compare case
as written, so another spelling never gains an allow. On a disk that keeps
case, `DOCS/` and `docs/` are different folders and are compared as written.
A deny or ask path pattern is also matched without regard to case on macOS and
Windows, and on any disk found to open a name in another case (a Linux
case-folding folder), so `write(**/VAULT/**)` protects a folder the disk holds
as `vault`. On macOS and Windows this holds even on a volume formatted to keep
case, where it can refuse or ask about a folder of another name that differs
only in case; that only tightens. An allow pattern's case is never folded.

A pattern's Unicode form does not matter for deny and ask rules. A name with
an accent can be spelled in two forms, NFC (`é` as one character) and NFD
(`e` then a combining accent); Finder copies names in NFD. Deny and ask path
patterns are also compared with both the pattern and the path in NFC, so
`write(**/café/**)` refuses a write to `café/` in either form. This only adds
matches. Allow patterns are compared as written: on a disk that keeps Unicode
form, such as most Linux disks, the two spellings are two different folders,
and an allow rule must not reach the one it does not name.

A folder that cannot be listed, such as a drop folder with no read
permission, cannot say how it spells what is inside it, so a name under it
keeps the spelling given.

On macOS and Windows, where the disk ignores case, a command's program name is
compared without case: `bash(whoami*)` denies `WHOAMI` and `Whoami`,
`bash(git tag*)` asks for `GIT tag`, `RM -rf` and `Git checkout -- .` are
destructive, and `Git status` is offered the scope `bash(git status *)`. Only
the program name is folded, and in a rule only its literal start, before any
`*`, `?` or space; arguments are compared as written, so folding only ever
adds a match to a deny or ask rule. On Linux, `GIT` is a different program and
is compared as written.

For `bash`, an allow rule with a pattern approves only a single simple command.
A command with `;`, `&`, `|`, a newline, `$(`, `${`, a backtick, `<`, `>`, `(`
or `)` anywhere in it, even inside quotes, falls through to a prompt, and no
"always allow" scope is offered for it. `bash` on its own and `bash(*)` still
allow every command. An allow rule whose own pattern holds that syntax, such as
`bash(cd x && go test*)`, can never match, and a warning names it at startup.
A rule for an interpreter allows whatever it can run: `bash(vim -es*)` allows
any command, since a vim script runs shell commands with `:!`.

A one-click "always allow" is offered only for a short list of well-understood
tools and subcommands; anything else can be approved once or allowed with a
rule you write. The list:

- `git` with `status`, `diff`, `log`, `show`, `branch`, `add`, `commit`,
  `switch`, `rev-parse`, `ls-files`, `blame` or `tag`, and `git stash list`
  or `git stash show`
- `ls`, `cat`, `head`, `tail`, `wc`, `pwd`, `echo`, `which`, `file`, `stat`,
  `du`, `df`, `grep`, `jq`, `diff`, `cut`, `tr`, `mkdir` and `touch`
- `npm` with `ls` or `outdated`; `pip` and `pip3` with `list`, `show` or
  `freeze`; `docker` with `ps`, `images`, `logs` or `version`

The scope is the program and its subcommand, as in `bash(git commit *)`, or the
program alone, as in `bash(ls *)`; for `git stash`, the one read-only word
after it too, as in `bash(git stash list *)`. The program must be spelled
exactly so (on macOS and Windows, without regard to case): `/usr/bin/git`, a wrapper or a `VAR=value` assignment in
front gets no scope. Nor does a command with `--eval` or `--exec` among its
arguments; for `git`, `npm`, `pip` and `docker`, any single-dash word
containing `c` or `e`, such as `git commit -c` (the tools without
subcommands take `-c` and `-e` as counts or patterns, so `wc -c`, `head -c`,
`du -sch` and `grep -e` keep theirs); nor one with a quote, backslash, `$`,
glob, brace or backtick in the words the scope keeps. Nor does a program
that `PATH` finds inside the workspace or an added directory (a planted
`./venv/bin/ls`), or behind a relative `PATH` entry. When no scope is
offered for a simple command, the reason names the allow rule that would
allow exactly that command, such as `bash(python3 -m unittest)`, for you to
add to `permissions.allow` if you want it. Interpreters, shells, package runners,
`make`, `npm install` and `npm test`, `go` (a `toolchain` line in `go.mod`
makes it run another go binary), `yarn`, `pnpm`, `cargo` and `kubectl` are left
off because a word on the line, or a file the agent can write, can make them
run anything. With corepack enabled for npm, the `packageManager` field in
`package.json` chooses the npm that runs, so the sandbox is the boundary there
too.

A scope covers every later command that starts with its words, so the list
leaves out tools whose harmless calls sit beside ones that throw work away:

- `git restore`, `git checkout` and `git stash` get none, because
  `git restore --staged x` sits beside `git restore .`, `git checkout main`
  beside `git checkout .`, and `git stash list` beside `git stash drop`. Only
  `git stash list` and `git stash show` are offered, each on its own.
- `cp`, `mv`, `uniq` and `tree` get none: each can write over a file (`uniq`
  with an output file, `tree -o`), and `mv` can move a folder out of the
  workspace, which is a delete.
- The discarding forms Abhed knows of the subcommands that keep a scope, such
  as `git branch -D` or `-f`, `git tag -d`, `git switch --discard-changes`
  and `--output=<file>` on `git log` or `git stash list`, are destructive
  commands (below). A destructive command offers no scope and always
  confirms, whatever was allowed before.
  That check reads the command's words, so it is best effort: the sandbox
  and your commits are what protect work, not the scope.

git is on the list, but it runs the repository's own hooks and config: hooks,
`core.fsmonitor`, diff and filter drivers and the pager. An agent that can
write under `.git`, with the write tool, can make an allowed git command run
code of its choosing. The sandbox tier is what contains that, not the scope.

A rule you write by hand for a script approves the file by name, not its
contents: `bash(python3 script.py *)` also allows whatever the agent later
writes into `script.py`.

Deny and ask rules match the whole command or any command inside it: split on
those operators, taken out of substitutions and subshells, and past leading
`VAR=value` assignments, redirections and wrappers such as `sudo`, `env`,
`nice`, `nohup`, `timeout`, `xargs`, `exec` and `command`, and the command
after `find`'s `-exec`, `-execdir`, `-ok` or `-okdir`. That split does not
parse the shell's quoting, so it can only add a denial or a prompt.

The rules also read each command a parser for **bash** finds, with its
quoting undone and `$'…'` decoded as bash decodes it (a NUL ends it): in
function bodies and subshells, in an alias's value, and in literal text given
to `eval`, `trap`, a shell's `-c` (or `su`, `flock` or `script -c`), or a
shell's here-string or heredoc. A command the parser cannot read, such as one
with an unclosed quote, is refused while any deny rule for `bash` has a
pattern, and asks while an ask rule does. So is one that runs text no rule
can read: a shell reading its commands from a pipe or a file
(`… | bash`, `bash < f`, `exec < f; bash`, `source /dev/stdin`), `hash -p`,
`enable -f`, or an alias whose value is built from an expansion. A shell's
input is its last redirect of fd 0, as bash applies them left to right; a
here-string on another fd is not its input. The parser reads bash. The
container tier runs a command with `/bin/sh`, which on its Debian image is
dash; where dash reads a command differently, the text checks beside the
parser still apply, but the reading is bash's. The sandbox, not the pattern,
is the boundary. A command too long or complex to split in full (over
64 KiB, over 1,024 parts, or a wrapper with too many readings) is always
asked about while any deny or ask rule for `bash` has a pattern, in every
mode.

Deny and ask rules also read the shell scripts a command runs, from the
folder the agent's command starts in: a shell's script (`bash ./s.sh`,
`sh -e -- s.sh`), `source f` and `. f`, a program named by a path that is a
shell script (`./tool` with a `#!/bin/sh` line, or no `#!` line at all), and
the scripts those source, two levels deep. So `bash ./s.sh` is refused under
`bash(curl *)` when `s.sh` holds `curl`. A script is refused while a deny
rule for `bash` has a pattern, and asked about otherwise, when it:

- does not exist, or is named from `~`
- may be written by the same line before it runs: a redirection
  (`echo … > s.sh; sh s.sh`), `cp`, `mv`, `ln`, `install`, `rsync`, `tee`,
  `dd of=`, `sed -i`, `perl -i`, `curl -o` or `wget`, including a copy into
  its folder (`cp sub/s.sh . && ./s.sh`) and any file those name by an
  expansion or a pattern (`cp "$f" s.sh`, `cp sub/*.sh .`) and any folder's
  contents copied without naming its files (`cp -r sub/. .`, `cp -T sub .`,
  `rsync -a sub/ .`, `ditto sub .`, and `cp -R sub/ .`, which copies the
  contents on macOS); names are compared in any case, and a write to a
  folder on the script's path counts
- is named by a relative path on a line that may change folder first: `cd`,
  `pushd`, `popd`, `env -C`, `sudo -D`, `find -execdir`, or a script the
  line sources that runs `cd` (`cd sub && ./s.sh`, `. ./setup.sh && ./s.sh`).
  The check does not follow the folder; it refuses. A script named by an
  absolute path is still read.
- is one of more than 16 scripts the line runs
- is sourced by a name without a folder (`. s.sh`, `source s.sh`) on a line
  that sets `PATH` in any way (`PATH=sub . s.sh`,
  `export PATH=sub:$PATH; source s.sh`, `read PATH`), as bash then looks
  for it on that `PATH` first

A line that sets `PATH` to folders it names (`PATH=sub:$PATH tool`,
`export PATH="$PWD/bin:$PATH"`) has each program it names without a folder
read as a script from each of those folders, an empty entry and `$PWD` being
the start folder; one the line may write first there is refused. A file a
shell runs as it starts is read as a script the line runs: `BASH_ENV=f`,
`ENV=f` (also through `env` or `export`), `--rcfile f` and `--init-file f`.
So is each start-up file a shell the line starts reads from a folder the
line sets `ZDOTDIR` or `HOME` to, by how it starts: zsh reads
`$ZDOTDIR/.zshenv` (`$HOME` when `ZDOTDIR` is not set) on every start,
`zsh -c` included, and `.zprofile`, `.zlogin` and `.zlogout` as a login
shell (`-l`, `--login`, `-o login`) and `.zshrc` as an interactive one
(`-i`); bash reads `.bash_profile`, `.bash_login`, `.profile` and
`.bash_logout` as a login shell (not with `--noprofile`) and `.bashrc` as an
interactive one (not with `--norc`); sh, dash and ksh read `.profile` as a
login shell, and ksh and mksh `.kshrc` or `.mkshrc` as an interactive one.
A plain `bash -c` or `sh -c` reads none; a script the line runs may be
zsh's, so `.zshenv` is read for it. One that is not there is skipped, and
one that cannot be read, or that sets `ZDOTDIR` or `HOME` again, is refused
or asked about. A start-up file or folder named by an
expansion (`BASH_ENV=$f`, `ZDOTDIR=$d`), or set by a command that sets a
variable by name (`read BASH_ENV`, `read ZDOTDIR`), is refused or asked
about as the scripts above are. A shell started with the `HOME` the
sandbox gives commands reads its start-up files there, which an earlier
command may have written, so those are read too, by the same rules (a
`#!/bin/zsh` script reads `.zshenv`). That
`HOME` is the host's own on the process tier and none, the fence's private
home folder under the fence, and, in a container, a microVM, a driver's
sandbox or a control-plane sandbox exec, a folder the host cannot read:
there a shell that would read start-up files from it (`zsh -c`,
`bash -l -c`) is refused under a deny rule and asked about under an ask
rule.

One the rules cannot read in full, because it is over 64 KiB, does not
parse, runs text built when it runs (`eval "$x"`), or itself runs a script
by a relative path after a `cd`, is asked about, in every mode.
A binary, or a script for another interpreter (`#!/usr/bin/env python3`),
is not read. A script is read when the decision is made, so one changed
between then and the run is not seen; the sandbox is the boundary there.

A deny pattern matches the words, so it is easy to step around.
`bash(curl *)` does not catch any of these, and every one of them runs curl:

- a script another program reads (`python3 x.py`, `node x.js`, `make`), a
  shell script over 64 KiB, one the agent's own commands judge from a folder
  the server cannot see (an `:exec` in a sandbox), a program the line
  builds before it runs it (`make && ./bin/app`), or a script another
  program on the line writes or unpacks (`tar x`, `unzip`, `git checkout`,
  `patch`, `python3 -c`)
- a program or script found through a `PATH` entry built from another
  expansion (`$HOME/bin`, `$(go env GOPATH)/bin`, `~/bin`), or through the
  `PATH`, `BASH_ENV` or `ENV` the command inherits rather than sets (a
  command on the host drops an inherited `BASH_ENV`); and the start-up files
  of a shell another program starts (`make`, a `#!/usr/bin/env -S zsh`
  line the check does not read as zsh's), or of one a line starts with
  options built when it runs
- a command given to `su` on its input, or to another program that runs a
  command it is not told to on its line, such as one git runs from its
  configuration (`git -c core.pager='curl x' log`, `GIT_SSH_COMMAND`,
  `core.sshCommand` or `git clone -u`)
- a shell another program starts with its input: `… | xargs bash -c`,
  `… | xargs -I{} bash -c {}`, a pipe into a function that runs a shell
  (`f() { bash; }; cat f | f`), or a process substitution written to
  (`echo 'curl x' > >(bash)`, `tee >(bash)`)
- git's own options before the subcommand: `bash(git push *)` does not
  match `git -C d push`, `git --no-pager push` or `git -c x=y push`; write
  the rule for each form or deny `bash(git *)`. The destructive step still
  finds a force push past them
- a flag spelled another way: `bash(rm -rf *)` does not match
  `rm --recursive --force x`, `rm --rec -f x`, or a flag hidden in an
  option's value, because no rule knows each program's own flag grammar

A deny or ask rule that names flags after the program and any literal
subcommands also holds with those flags moved or split: `bash(rm -rf *)`
matches `rm x -rf`, `rm -r -f x` and `rm -v x -f -r`, and
`bash(git push --force *)` matches `git push origin --force`. The command
must have every flag the rule names, anywhere before `--`, and its other
words, in order, must match the rest of the pattern. This reading only adds
matches, and allow rules do not use it. For the destructive commands
themselves, the destructive step and the sandbox remain the control.

A program named by a path (`/usr/bin/curl x`, `./curl x`) is also matched by
its base name, so `bash(curl *)` holds for it; an allow rule still sees only
the path as written. A quoted or escaped name (`'curl' x`, `c\url x`) is read
as the program,
and literal text another command runs is read as the commands it is:
`bash -c 'curl x'`, `eval 'curl x'`, `trap 'curl x' EXIT`,
`bash <<< 'curl x'`, `su --command`, `flock`, `env -S 'curl x'`,
`watch 'curl x'`, a command under `find -exec`, `chrt`, `taskset`, `chroot`,
`unshare` or `timeout inf`, and the value of git's `--exec`,
`--upload-pack` and `--receive-pack` and the command of an `ext::` URL on
git's line, which also confirm as destructive.
Such text built from an expansion is refused while a deny rule for `bash`
has a pattern. So is any git argument built from an expansion once the line
enables the ext protocol (`protocol.ext.allow` or `protocol.allow` in `-c`,
or `GIT_ALLOW_PROTOCOL`). An `ext::` in the text of `git commit`, `grep`,
`log`, `show`, `shortlog`, `tag` or `notes` is not read as a URL. Every
command the agent runs gets `protocol.ext.allow=never` as git environment
config, so an `ext::` URL from `.gitmodules` or a repository's own config
(`git submodule update`, `git fetch o`) is refused by git itself; only `-c`
or `GIT_ALLOW_PROTOCOL` on the line, which the checks read, lifts it. The
same environment config sets `core.fsmonitor=false`, so a repository's
config cannot make `git status` run a command.

A command git runs from configuration it reads itself is still not on the
line for any rule to see: `core.sshCommand`, `core.hooksPath` and the hooks,
`diff.external`, a filter driver named in `.gitattributes`, or
`credential.helper`. On the line, `-c` and an alias confirm as destructive
in a session and are refused on `:exec`; `core.pager` runs only when git
writes to a terminal, which an agent's command never is. For the rest, keep
the agent from writing `.git` (the sandbox's git protection) and limit
where its commands can connect (`sandbox.network`), so a command git runs
cannot reach out.

Words the shell builds are read as it would build them: line continuations
are joined, and `$IFS`, `${IFS}`, tabs, `$'…'` and braces such as
`{curl,x}` are read as the words they make, so `c${IFS}url` is matched as
`curl`. Braces are read as bash expands them, lists and sequences alike,
nested or side by side (`mkdir -p a/{b,c}/d{1,2}`, `{c..c}url`,
`echo {a..c}{1..2}`), and are not asked about for that. Where bash 3.2
reads a sequence apart from later versions (an increment such as
`{1..9..2}`, zero padding such as `{01..10}`), the rules see both readings.
Braces are read so at any depth: in text given to `eval`, `trap`, a shell's
`-c`, a here-string or a heredoc (`bash -c '{curl,x}'`,
`eval 'c{u..u}rl x'`), in function bodies and in substitutions. A `for`
loop's list (`for i in {1..2000}`) runs no command and is not asked about.
A command whose braces make more than 16,384 words in all is asked about
while a deny or ask rule for `bash` has a pattern. A command that sets `IFS` to a separator other than blanks is also read
split on it, so `IFS=_; curl_x` is matched as `curl x`. Where such a command,
or one that sets `IFS` in a way the text does not show (`IFS=$v`, `read IFS`,
`${IFS:=_}`), then expands anything (`IFS=_; c=curl_x; $c`), the words that
run cannot be read, and it is refused while any deny rule for `bash` has a
pattern, in every mode. `IFS` is found however it is spelled or set: through
quotes, backslashes and `$'…'` (`eval I""FS=_`), in arithmetic
(`(( IFS = 1 ))`, `$[ IFS = 1 ]`), named to a command that sets a variable
(`read IFS`, `printf -vIFS`, `for IFS in`), and named by a word built from an
expansion where it could become `IFS`: given to `eval` or a shell's `-c`,
or as the name a setter is given (`read ${v}FS`, `printf -v "$v"`). A prefix
to `read` (`IFS=, read -r a b`), `IFS` set only to blanks (`IFS=$'\n'`) and
`IFS` as another command's argument (`grep IFS f`) and a name that only holds
the letters (`(( MYIFS = 1 ))`, `read MYIFS`) are not refused. A prefix to
`read` is not asked about either, as it splits only what `read` reads, so
`IFS=, read -r a b < f` runs on `:exec`. The refusal
says how `IFS` was set: to a separator the text shows, in a `for` loop, by
arithmetic, to a value the text does not show, or by text built from an
expansion. Some
commands that set nothing are refused with an expansion after them, because
the text cannot tell them apart: a setter's name in quoted text
(`echo "read IFS"`), any `x=IFS`, `IFS` beside a blank and `=` (`[ IFS = x ]`),
and `eval` or `-c` text with an expansion in it.

The program is found past assignments, wrappers, option values, durations
and redirections (`2>/dev/null`, `<<< x`, `>| f`), so `2>/dev/null curl x`
and `nice -n 1 c\url x` are matched as `curl x`, and in function bodies and
`eval`, `trap` and `-c` text. A program named by an expansion that edits its
value, such as `${x%/}`, `${x//_/ }`, `${x:-curl}` or `${!v}`, cannot be
read at all: `x=curl_x; ${x//_/ }` runs `curl x`. It is refused while any
deny rule for `bash` has a pattern, in every mode. Any other expansion in
the program's word, and an unquoted glob (`cur? x`, `c*l x`), confirms; see
[the destructive step](#the-order).

Deny rules guard against mistakes, not against a command written to get past
them. What a command can reach is decided by the sandbox, and that is the
boundary to rely on. A deny refuses one call, not what it was for: the agent
may reach the same end another way, so deny the means only where the
sandbox cannot.

A malformed rule is **refused at startup** rather than silently matching
nothing — for a deny rule, quietly accepting one that can never fire tells you
that you are protected when you are not.

### Policy layers from an edition

On a server people sign in to, an edition can add rules of its own to every
session of a person, as one more layer: the Team edition adds the rules of
the workspace the session's owner belongs to. Such a layer only tightens
what the configuration allows:

- Its deny and ask rules are added to the configured ones and hold in every
  mode, bypass included, as configured ones do. A session rule or a "don't
  ask again" answer cannot lift them.
- Its allow rules are added only where the managed configuration leaves
  allow rules open. Under a managed file that sets any `permissions`
  setting they are set aside, and the record says so.
- Every rule is read with the same parser as a configured one. A layer the
  server cannot read, or whose rules do not parse, refuses the session
  (`503`): a session never runs with fewer rules than its layer holds.
- The layer is recorded as a `policy.layer` event right after
  `session.started`, with its name, revision, the SHA-256 of its rules, the
  rules applied and those set aside. A continued session records it again
  before its next turn when it has changed. A decision one of its rules
  made carries `layer` beside `rule`, its reason names the layer
  (`denied by workspace pay rule bash(curl*)`), and HawkEYE counts the
  decisions each layer made.
- `GET /v1/sessions/{id}/permissions` is the server's `/permissions`: the
  session's mode and every rule in force, each with its layer (`managed`,
  the edition's layer, `user`, `workspace`, `flag`, `default`, `session`,
  `session (always allow)`), and the layer's rules set aside.

An edition supplies the layer through `server.Options.SessionPolicy`
([control-plane contract, §12](../architecture/control-plane-contract.md#12-extension-points-for-workspaces-implemented)).

## The order

Every call goes through the same steps, and the order is the design:

0. **Arguments** — before any rule or hook, a call's arguments are decoded
   strictly and written out once in a canonical form. A call whose
   arguments are not one JSON object, name the same key twice in any case
   (`command` and `Command`), spell a declared argument in another case
   (`Content` for `content`), or give a tool a key the rules read
   (`command`, `path`, …) that it does not take, is refused at step `args`. A
   built-in tool drops any other key it does not declare, the record
   lists them in `dropped_args` (each clipped to 64 characters, at most 32,
   then `+N more`), and the result tells the model `[ignored: …]`; an MCP tool whose schema sets
   `additionalProperties: false` refuses them instead. Every later step,
   and the tool itself, reads those same canonical arguments
1. **Hooks** — extensions, next, so they can veto. A hook's refusal is final
   here. A hook's ask takes effect after the deny rules and plan mode, so it
   never turns a refusal into a question, and a hook's "allow" is no opinion
2. **Deny rules** — absolute for every tool call, the agent's and a person's; they survive every mode, including `bypass`. In the workbench's interactive shell, which the sandbox bounds, they screen each line as typed, best effort ([the workbench](16-workbench.md))
3. **Plan mode** — in `plan`, a mutating call is refused here, before the
   destructive and ask steps, so a destructive command or an ask rule is not
   put to a person who might accept it. Read-only calls go on as before
4. **Destructive commands** — force push, hard reset, disk writes, fork bombs
   and similar always confirm, in every mode, because there is no undo. For git
   these are the forms that discard work which Abhed recognises: `git restore`
   of the working tree (anything but `--staged` alone), `git checkout` with a
   pathspec (`.`, `:/`, a glob, a path after `--`, two operands, or one
   operand named like a file, such as `main.go`) or `-f`,
   `git commit --amend`,
   `git switch --discard-changes` or `-f`, `git stash drop` and `clear`,
   `git branch -d`, `-D`, `-f`, `-M` or `-C`, `git tag -d` or `-f`,
   `git worktree remove -f`, `git clean` other than a dry run,
   `git reset --hard`, `git push` with `-f`, `--force`, `--delete`, `--mirror`
   or a `+` or `:` refspec, `git read-tree -u`, `git checkout-index -f`,
   any `git update-ref`, `git archive -o`, and `--output` on any git
   command, such as `git diff`, `log`, `show`, `stash show` or
   `stash list`. Where the check
   cannot tell what will run, it takes the command as destructive. A git
   command with an argument made when it runs (`git reset "$1"`,
   `git reset $x`, `git reset "$@"`, `git reset --h*`) is one, for a
   subcommand with a form that discards work, as is one that writes
   `--output` with an argument an expansion or glob could make an option
   (`git diff $x`). A quoted pattern (`git tag -l 'v*'`) and an expansion
   after a literal `--name=` (`git log --since="$d"`) are not; but an
   option's separate value and a revision named by one still are, so
   `git log -n "$N"`, `git diff "$base"`, `git show "$sha"`,
   `git checkout -b "$b"` and
   `git push origin "$(git branch --show-current)"` confirm. Write the
   value as `--name=value`, or the
   revision out, to run them without a question. A program
   named by an expansion is one: `$x`, `$(which tool)`, and an unquoted one
   in a path, `$HOME/bin/tool` or `$(pwd)/run.sh`, since its value may split
   into a program and its arguments (`$(printf 'curl x ')/` runs `curl`).
   These confirm in every mode, `bypass` included, and are refused in
   headless `-p`. To run a tool by such a path without a prompt, quote the
   directory, `"$HOME"/bin/tool`, `"$(pwd)"/run.sh` or `"$GOPATH/bin/x"`, or
   write the path out: a plain expansion inside double quotes and followed by
   a `/` can only be a directory. That holds for one word only: `"$@"/x`,
   `"$*"/x` and `"${a[@]}"/x` still confirm, as `"$@"` is a word per
   parameter. So are a git subcommand git does not have, which is an alias
   or an extension (`git wipe`, whether the alias is in the repository's
   config or set with `-c alias.wipe=…`), any `-c` or `--config-env` that
   sets an alias or an include, and a git whose name comes from a
   substitution that names git
   where it is the program (`$(which git) reset --hard`,
   `` `which git` checkout -- . ``), read with the words that follow it. The
   program is found past shell keywords (`then`, `!`, `{`, `while`), `eval`,
   and runners such as `sudo -u bob`. A false match only asks, but no allow
   rule, scope or mode approves it: an extension such as `git lfs` or
   `git flow`, or your own alias such as `git st`, asks every time, and is
   refused in headless `-p`. Run it by its full subcommand, or outside
   Abhed, or name an extension you trust in `permissions.git_extensions`
   (`["lfs"]`) in your own configuration, a `-settings` file or the managed
   one: it then runs without this question. Deny and ask rules still hold
   for it, and every other part of the command is still checked. The name
   runs whatever `git-<name>` is installed, or, where none is, an alias of
   that name a repository's configuration defines, so opt in only to an
   extension installed on the machine. A workspace's untrusted
   configuration cannot opt one in, and when the managed file sets the
   permissions without naming its own, yours are set aside with a warning.
   Long options are recognised shortened, as git accepts them (`--del`, `--har`), and a later
   `--no-dry-run` or `--no-staged` takes back the flag that made a command
   safe. They are found wherever git takes options, among the operands too, in
   any part of a chain, after git's own options such as `-C dir`, and behind a
   wrapper; a single command with more than 16 words named `git` is taken as
   destructive. The list is best effort, like the rest of this step, and the
   sandbox is the boundary. Outside git, `unlink FILE` and `rmdir -p` confirm
   too. It misses, among others: `git checkout FILE` with one word and no
   file extension (`Makefile`), which git reads as a branch first and
   otherwise as a path; `--amend` built when the command runs; and deletion
   through any program the list does not name (a script, `python3 -c`). The
   list cannot be complete: it is a speed bump for the common cases, and the
   sandbox, deny rules and committed history are what hold. A command that sets
   `IFS` to a separator, or in a way the text does not show, and then
   expands anything is taken as destructive
   - no scope is offered for a destructive command, and none remembered
     satisfies it
   - a command too long or complex to split into its parts asks while a patterned
     `bash` deny or ask rule exists, so no mode or allow rule can approve it unchecked
5. **Ask rules** — force a prompt even where a later allow would match. They
   offer no "always allow", and no scope chosen earlier in the session
   satisfies them: an ask rule asks every time
6. **Mode**
7. **Allow rules**, then a default: read-only proceeds, mutations ask

The rules a person adds with `/permissions` are evaluated with the configured
ones, after them, in each of the deny, ask and allow lists.

Two consequences worth stating plainly. **A deny rule cannot be overridden** by
a mode, an allow rule, an extension, or an operator's own bypass. And **an
extension may veto but never permit**: it can block a call or force it to a
prompt, and cannot turn a denied action into an allowed one. A permission gate
an extension could remove would not be a guarantee.

## What the agent may touch

The file tools write only inside the workspace it was started in.
`additional_dirs` extends that, and is set by the operator — never by the
model. A shell command is bounded by the sandbox instead, which on the process
tier also lets it write temp folders and, on macOS, toolchain caches
([Sandbox](02-configuration.md#sandbox)).

`.abhed/` — the configuration, the users file and the keys, in the workspace
and in the home directory — is out of the agent's reach in every mode. The file
tools and the server's file endpoints refuse it by any spelling the disk
resolves to it (`.ABHED` on a case-insensitive disk, a file or folder symlink
to it, even one swapped in while the path is checked, or a hardlink to its
users, config or secrets file), glob, grep and the index pass over it and
follow no file symlinks, and the process sandbox hides it from commands, so
no prompt can talk the agent into dropping a deny rule or adding a user for
the next start. It is a boundary rather than a rule, because a rule lives in
the file it would be protecting. A hardlink to another file in a state
directory is recognised for the first 4,096 files and folders there.

The `write` and `edit` tools also refuse anything inside a `.git` folder, and
a `.git` file, in any case and through a link, in every mode: a hook or a
config line written there runs a program at the next git command, Abhed's
own or yours. Git commands still change the repository. Commands are not
held to this by the file tools. Every surface (the terminal CLI, `abhed -p`,
`abhed rpc`, Abhed Studio and every session `abhed serve` runs) also keeps
out of its commands' reach what git reads in a git folder as configuration
or follows elsewhere: `config`, `config.worktree`, `hooks`, `commondir`, `gitdir`,
`info/attributes` and `objects/info/alternates`, in every git folder, its
linked worktrees' (`.git/worktrees/*`) and its submodules'
(`.git/modules/**`). In a served session that covers the agent's commands,
its background shells, `!` commands and the workbench terminal, in both of
its modes. The none tier holds nothing. For the terminal CLI, `abhed -p` and
`abhed rpc`, `"sandbox": {"protect_git": false}` turns this off, and `true`
also refuses the fence in `landlock_only` mode, which cannot hold these files
(unset, that fence runs and says so); Studio and `abhed serve` always hold
them.

An "always allow" for a git command (`bash(git status *)`) is remembered
with a fingerprint of what git reads in the workspace's repositories and
added folders' (the git folder's `config`, `config.worktree`, `hooks`,
`commondir`, `gitdir`, `info/attributes` and `objects/info/alternates`, a
linked worktree's common folder's too, the `.git` entry itself) and of your
`~/.gitconfig` and `~/.config/git/config` and `attributes`. When any of them
changes, the next such command is asked again, wherever the change came
from. A grant restored when an editor reloads a session is asked again too.
The repository holding the folder the shell is in counts too, so a nested
repository the shell has moved into with `cd` is part of the fingerprint.

On macOS this is by pattern, at any depth and for repositories and files
made later. The folders holding those files (`modules` and each folder in
it, `worktrees` and each folder in it, `info`, `objects`, `objects/info`)
cannot be moved, removed or made by a command, though what they hold stays
writable, so a command cannot move one aside and put a link to a copy in its
place. Nor can a name be made beside a submodule's git folder named with
slashes (`modules/lib/y` beside `modules/lib/x`) in the workspace's own
repository; in a nested repository it can, as Abhed looks for those names
only in the workspace's own. Git inside the sandbox therefore cannot make a
linked worktree or a submodule's git folder: `git worktree add`, `git
worktree remove` and `git submodule add` or `update --init` for a submodule
not yet checked out fail there (`git submodule add` stays so; run it
yourself; the others are run on the host, as below). A `hooks` (or other of
those files) that is a link is held where it leads as well, as is the
workspace's own `.git`, or a `modules` or `worktrees` folder or a folder in
either, that is a link; a linked `.git` is then read-only throughout, so git
cannot commit there. So is the folder a nested repository's `.git` link
leads to, for nested repositories the walk below reaches (six folders down,
at most 20,000 folders): git cannot commit there either, so replace such a
link with the folder itself.

On every tier, plain `git submodule update` on a submodule already checked
out fails inside the sandbox too, as git rewrites `core.worktree` in the
submodule's protected config.

So on the process tier and the fence, a command that is exactly `git
worktree add`, `git worktree remove`, `git worktree prune` or `git submodule
update` (plain words, the usual options only, no second command), already
approved as any command is, runs through host git instead, with the settings
Abhed's own git uses: hooks, fsmonitor, filters and every transport but https
off. A path it names outside the workspace refuses it. Host git is not
held by the sandbox's network rules, so with `allow_network` off or the
egress allowlist on, `git submodule update` is brokered only with
`--no-fetch` (or `-N`) and runs with no transport at all: it checks out
commits already fetched and fails on a submodule not yet cloned. Without
`--no-fetch` it is refused; fetch the submodules yourself, outside Abhed. Another spelling, an
option outside the usual ones or a line with more on it runs in the sandbox
and may fail there. The container and vm tiers do not broker them; run them
yourself there. `git worktree remove --force` deletes the worktree's files as
you asked, then its folder under `.git/worktrees`, instead of deleting the
files and then failing.

Under bubblewrap and in a container (the vm tier too) it is by binding
read-only those found when each command starts; an empty hooks folder or
config file is made first where a repository has none, and an empty
`config.worktree` where git would read one. The workspace's own git folder,
its submodules' and its linked worktrees', with each one's `.git` file in
the work tree, are found first; other repositories by a walk down six
folders, past `node_modules` and `.abhed`, that enters at most 20,000
folders and looks at each folder's `.git` before what the folder holds.
Repositories a session has found stay protected after a command fills the
workspace with folders; one past that bound and not found before is not
protected, and reaching the bound is recorded once a session as
`sandbox.git_walk_bounded`. Where git protection is required (Studio,
`abhed serve`, or `"protect_git": true`), reaching the bound refuses the
command instead, recorded with `refused: true`: remove folders that do not
belong there, or run Abhed in a smaller folder. Modules holding more than 20,000 entries refuse
the command, as no repository has them. A `hooks` (or other of those files)
that is a link cannot be bound, and a command could point it elsewhere, so
each command is refused until it is replaced with what it points to; so is a
`.git`, `modules` or `worktrees` folder, or a folder in either, that is a
link. `git worktree remove` fails there, as the worktree's folder is bound.
Git refuses an empty `commondir`, so none can stand in for a missing one: a
`commondir` found where git never writes one, or a linked worktree's that
names anything but its repository's git folder by `../..` or by its path,
with no symbolic link on the way, is moved to `~/.abhed/quarantine`, that
command is not run, its error says what was moved where, and the record
gains `sandbox.git_planted`. Until the next command starts, git you run
yourself in that repository would follow it. Binding holds only what exists
when a command starts: within one command, a command can make a repository
(`git init new`), give it a config naming an fsmonitor and add it to the
parent as a submodule entry (`git add new`), or make a `.git/modules/<name>`
that a later `git submodule update --init` reuses, and git run in the parent
outside Abhed may then run that program. On macOS no `.git` can be made in
the workspace, nor a folder under the workspace's own `modules` (a nested
repository's slash-named submodules excepted, as above), but Seatbelt checks
a rename only at its two ends, so the same gap is open there: a command can
build a repository in the writable temp area, with a config naming an
fsmonitor, move it into the workspace and add it as a submodule entry, or
move it over a nested repository that is not a submodule. Moving a
registered submodule's work tree is refused.

Before each command, a repository (a `.git` folder or file) found that was
not there at the session's first command is checked: a setting in its
`config` or `config.worktree` that `git init`, `git clone` or a submodule
would not write (anything beyond the core settings, remotes, branches,
submodule URLs, extensions, user, `init`, `pull`, `fetch`, `gc`, `index` and
`lfs` format) is taken out, the file as it was copied to
`~/.abhed/quarantine`, a hooks folder holding anything but git's samples is
moved there, the command is not run, its error says what was done, and the
record gains `sandbox.git_planted`. Abhed changes only a `.git` folder made
during the session. A new `.git` file naming a git folder outside the
workspace or under `.abhed` (in any case), a new `.git` file naming any git
folder (the workspace's own, a submodule's) whose config or hooks name a
program, a git folder there at the first look moved to a new place, a
repository found only after a first look that stopped at its folder bound,
and a new repository whose `config` is a link are never changed by Abhed:
every command is refused, naming it, until you remove it or those settings;
so is one Abhed could not clean (a read-only git folder, say), until it can
be. A `.git` file naming a git folder that names no program, as `git
submodule add` and `git worktree add` write, is let through. A config is replaced rather than
written through, so a hard link to it keeps its other contents. This runs
under bubblewrap, in a container and on macOS; under the fence in
`mount_namespace` mode too. Until
the next command starts, git you run yourself there would read the setting,
and a `.git/modules/<name>` made for a later `git submodule update --init` is
not checked. A tool such as husky that sets `core.hooksPath` in a repository
the agent cloned has it taken out as well; set it yourself. Before running
git in a workspace a session has used, check `git status` and `git diff --submodule`
for submodule entries you did not add. A root status never enters a nested
repository that is not a submodule, so before running git in one, check its
`.git/config` for settings that run a program, such as `core.fsmonitor` and
`core.hooksPath`.

Abhed's own git (the prompt's branch, worktrees, a resolved issue's commit)
runs inside the session's sandbox on the process tier and the fence; see
[security posture](../trust/security-posture.md). It refuses to run while a `commondir` names
anything but the repository's own git folder, by `../..` or its path
through no symbolic link, while a work tree's `.git` file reaches its git
folder through a symbolic link (`git worktree repair` writes it afresh),
and where a `.git` is present but git cannot say where the git folder is.
The fence holds the same files in its `mount_namespace` mode: each command's
mount namespace binds them read-only and pins the folders holding them, a
planted `commondir` is taken out before the next command, which is refused,
and a linked git folder, or a held file with a second name, refuses the
command. In `landlock_only` the fence cannot hold paths inside the writable
workspace, so Studio and `abhed serve` are refused there, as is the CLI with
`protect_git: true`.

The command sandbox guards `.abhed/` by path, not by file: a hardlink to a
state file elsewhere in the workspace is an ordinary path to it, which a
command could rewrite. The agent cannot make one on macOS, but one that
already exists would carry writes through. So Abhed refuses to start — the
CLI, the server, `abhed rpc`, `abhed acp` and `abhed resolve` — and
`abhed doctor` fails, when a state file has more than one name (the state
of every folder above the workspace counts too, so a run started in a
repository's subfolder checks the repository's `.abhed`; there only the
users, config and secrets files you could rewrite count, and a
world-writable sticky folder such as `/tmp` is skipped, so a workspace
directly in `/tmp` leaves the folders above it unchecked: start Abhed in a
folder of your own), and a
configuration file with more than one name is not loaded at all. The message
names the file and how to fix it: find the other name with
`find / -xdev -samefile <file>` and remove it, or give the file a single name
again with `cp -p <file> <file>.new && mv <file>.new <file>`. Link counts are
not read on Windows.

A users file set with `auth.users_file`, or a secrets file set with
`ABHED_SECRETS_FILE`, is refused by the file tools and the server the same
way. Keep it outside everywhere commands can write — the workspace, added
directories, temp folders and toolchain caches — or under the workspace's or
the home directory's `.abhed/` (a real folder, not a link elsewhere): Abhed
refuses to start otherwise, because a command could move a folder above it.

On the container and vm tiers, an empty folder is mounted over the
workspace's `.abhed/` and a configured state path inside a mount, and
`/dev/null` over a state file, so commands can neither read nor write them.

The operator edits these files by hand; the one exception is
`~/.abhed/skills`, which commands may read, since a skill can ship a script.

Content read from files, tool output and search results is **data, never
instruction**. Every event carries a trust tag, and untrusted content is marked
as such in the transcript and the console. A file that contains text shaped like
instructions is reported, not obeyed.

## When it asks

The question names the tool and shows the full command or the diff, says why
it is asked and which step of the policy decided, and offers the rule that
would allow such calls when one is offered:

```
╭─ Bash(git commit -m "Add the parser")
│ running a command needs approval in default mode · policy step: default
│ $ git commit -m "Add the parser"
│ Run this command?
│   1. Yes
│   2. Yes, and don't ask again for bash(git commit *) this session
│   3. No, and tell Abhed what to do instead (esc)
╰─ press a number to answer · esc to decline
```

For a command with no rule offered, such as `go test ./pkg/auth/`, the
question has Yes and No only: approve it once, or write the rule yourself.
The same holds for a call that matched an ask rule and for a destructive
command, which must be asked about every time and needs a second Yes. When a
skill's pipeline or a subagent asked, the question says which. How a key
counts as an answer is in [The terminal](20-terminal.md#approvals).

Text in the prompt comes from the model, so it is shown as written, not
obeyed. A carriage return, escape sequence, backspace, zero-width or bidi
character is printed as a marked escape such as `⟨\r⟩`, `⟨\e⟩` or `⟨U+200D⟩`, a
long run of spaces or tabs as a count such as `⟨32 spaces⟩`, and the prompt
warns that the call has hidden characters before the answers. The console and
the IDE do the same on their approval cards (a carriage return there reads
`⟨U+000D⟩`), and an editor over `acp` gets the same escapes in the permission
request's title, marked `(contains hidden or control characters)`.

The record names the scope on the approval that chose it (`granted_scope`),
and in the console and the API the person who answered (`approver`); a call a
remembered scope let through later is `by: session-scope` with that `scope`.
A call that is not approved, by a person or because no one can be asked, is
refused with its reason and, when one is offered, the rule that would have
allowed it:

```
This action was not approved (running a command needs approval in default mode).
It would be permitted by the rule bash(git commit *), which is not configured.
```

Rejecting feeds the reason back so the model adapts rather than rephrasing the
same command. A denial that says only "no" makes a model retry forever.

A call that could not succeed is not put to you. An `edit` or `write` of an
existing file that has not been read this session, an `edit` of a file that
changed on disk since it was read, and an `edit` of a file that is not there
are refused before the prompt with the reason the tool would give, so your
approval is not spent on them and the model is told to read first. When
another call in the same turn names the file, or a command runs before it,
the tool decides when it runs.

## Recovering

`/undo` reverts the last turn's file changes, and `/rewind` takes code and the
conversation back to before any prompt; each file put back is your write, held
to the same deny rules, and recorded. `/diff` shows what changed this session.
`/resume` replays a past session exactly, which is how you find out what an
agent did rather than what it said it did, and the next task continues that
session; see [Sessions and the local record](12-records.md).

## The monitor

**Not turned on in this release.** The engine has a place for a judge between
the deny rules and the ask rules, and the `monitor` package holds its rules,
but no command, configuration key or SDK option sets one. So no call is
reviewed by a monitor today, and no `monitor.verdict` is written. There is no
date for it: it will be turned on once it has been measured, not before.

What follows is how the judge works once one is set. Given the session's
remit, the agent's most recent stated reasoning, the proposed call and where
every host and path in it was first seen, it returns allow, ask or deny with a
reason. It runs on calls policy would ask about, on any mutation a mode waved
through, and on any call naming a host or path the user never mentioned; a
read the user asked for, allowed by rule, never reaches it.

Two rules make it safe to let it read the reasoning:

- **A verdict can only tighten.** An allow may become an ask or a deny, an
  ask may become a deny, and nothing moves the other way. A judge that could
  be argued into permitting would have to be kept away from the text that
  argues; here the worst a persuaded judge can do is nothing.
- **Absence is not consent.** A judge that is unreachable, slow or
  incoherent raises the call to ask — or to deny in a headless run, where
  nobody can answer.

Each consultation would be in the record as `monitor.verdict`, with the
decision before and after, the code, the rationale and the judge's version,
and a denial it caused would say so at step `monitor`. A subagent's calls
go to the same judge as its parent's, with a per-turn budget of their own.

## Secrets

A command that runs tests, calls an API or opens a pull request needs a
credential. It does not get one from your environment: the agent's commands
see only what the sandbox passes through. It gets one **by name**, from a store
the operator fills, and only under a rule that names it.

```
abhed secret set GITHUB_TOKEN      # value prompted without echo, or piped on stdin
abhed secret list
abhed secret rm GITHUB_TOKEN
```

Values live in `~/.abhed/secrets.json` (`ABHED_SECRETS_FILE` overrides), mode
600, outside every workspace and under the directory the agent's tools and
sandbox cannot reach. They are never in a config file.

The model sees the **names**, and asks for one on a single command:

```json
{"command": "gh pr create --fill", "secrets": ["GITHUB_TOKEN"]}
```

That command, and only that command, runs with `GITHUB_TOKEN` in its
environment. On the macOS process tier another command running at the same
time can read that environment; see [Configuration](02-configuration.md#sandbox). `k8s_login` names a token as `token_secret` and `ssh_connect` a
password as `password_secret`, and use it for that session's login only.
Whether any of them may is decided by a rule. Rules hold for the whole
deployment: on `abhed serve`, every session may name a secret its rules
allow, whichever user started it.

On a server that several accounts sign in to (local, proxy or OIDC
authentication), each account has its own store, and a session reads only
the store of the account it runs as: one account never uses or reads
another's secrets, nor the operator's. The stores are files in
`~/.abhed/secrets.d` (`ABHED_ACCOUNT_SECRETS_DIR` overrides; it must be an
absolute path), one per account, named by a digest so no account's name is
on disk; the tools and sandbox refuse that folder as they refuse the
operator's store. A local account made again under a removed one's name
starts with an empty store. The operator's store, which `abhed secret`
manages, serves scheduled runs only. A caller with no account (anonymous)
gets an empty store and cannot add to it. Each account manages its own
secrets through the API, names only, values never read back:

```sh
curl -X PUT  https://abhed.example/v1/secrets/GITHUB_TOKEN -d '{"value":"ghp_…"}'
curl         https://abhed.example/v1/secrets            # {"names":["GITHUB_TOKEN"]}
curl -X DELETE https://abhed.example/v1/secrets/GITHUB_TOKEN
```

A value is 8 characters to 64 KiB. Allow rules still decide which names a
session may use. Before 1.2.8 every account on such a server shared the
operator's one store.

A server embedding Abhed can give each account a store of its own choosing
through `server.Options.SecretsFor` instead: a session's commands, logins
and fetches then read only what it returns, and its record is redacted with
it and the operator's store.

```json
"allow": ["secret(GITHUB_TOKEN)"],
"deny":  ["secret(PROD_*)"]
```

A secret needs its own allow rule **in every mode**. `auto` and `bypass`
approve calls; they do not hand out credentials. A command that asks for a
name with no rule is refused, and the model is told which rule would permit
it. A deny rule wins over an allow rule, as everywhere else.

**Redaction.** The record is append-only, so a value that reached it could
never be taken out. Every event is checked before it is written: a stored
value, as written and, for 12 characters or more, in the encoded forms
below, becomes `[secret:NAME]`. Redaction matches the stored values; it is
not a pattern guessing at what a key looks like. A split or partial value
is not caught (see below).
When it fires, [HawkEYE](15-hawkeye.md) reports `secret-redacted`: the value
was caught, and the command or the model exposed it, which is worth knowing.

The reply and the reasoning stream as many fragments, and a value may be split
between two, so streamed text is held back by about the length of the
longest form a stored value is matched in (for a value of 12 characters or
more, its percent-encoded or hex form, two to three times its length) and
redacted together with what follows. The live view lags by that much, and
only when secrets are stored; the whole text is redacted as one event too.
A stored key file makes that lag visible, a few kilobytes of text, and values
that occur close together hold the text back until the last one is complete.
Text that cannot be redacted is never written as it was: it becomes
`[redacted: output withheld]`.

**File paths.** A `write` or `edit` whose path holds a stored secret is
refused, in every mode, whether the agent or a person at the workbench makes
it: the editor's save, and the explorer's New file, New folder and the new
name of a Rename, are checked the same way. `bash` is not: a command can still
create a file whose name holds a value the person typed or the agent built
(`touch "$TOKEN"`). The file then exists on disk under that name; every event
that names it, a later `ls` or `git status` included, is redacted as any
output is, in the encoded forms above too. Give such a command its secret
only when it needs it, and use the sandbox's egress allowlist so a name
cannot carry the value out. The path is matched as written, in its case, and
only against values of 12 characters or more, so a value such as `postgres`
does not refuse ordinary files. A value of 8 to 11 characters can therefore
still become a file name in a mode that approves writes without asking;
store longer values, or leave writes to ask.

Every way of running a session redacts with the same store: the terminal, the
server and the console, `abhed acp`, `abhed rpc`, `abhed resolve`, `abhed eval`,
subagents and the [SDK](09-sdk.md). There is no setting that turns it off. In
1.2.1 and earlier the SDK, and so `acp`, `rpc` and `resolve`, did not redact;
see the changelog.

A store that exists but cannot be loaded stops sessions from starting. That
covers a file that is empty (0 bytes), not valid JSON, readable by others,
unreadable, larger than 1 MiB, or not a regular file (a FIFO or a device). The
terminal, `abhed serve`, `eval`, `acp`, `rpc`, `resolve` and the SDK refuse to
start with an error that names the file and the fix, and `abhed doctor` reports
it as not ready. A missing store just means no secrets.

`abhed serve` checks the store when it starts. If the store breaks while the
server runs, a new session, chat or workbench, is refused with `503` and
`Retry-After`, and the server logs why. A resumed session, or one already
running, goes on, but every event payload it records is withheld until the
file is fixed, and it redacts again from then on. While the store cannot
be loaded, `GET /v1/health` answers `"status": "degraded"` with
`"secrets_store": "unreadable"`, still with code 200. The
same happens for the next conversation in a terminal that is already running.

Redaction follows the store for the whole session, as bash does: the store is
read again whenever the file changes, in each terminal conversation (`-p`
included), each server session (new or resumed), each SDK agent, each ACP
session, each rpc `start` and each eval task. A secret stored or changed during
a session is redacted from that moment on, with no restart, and a value seen
during the session stays redacted after it is changed or removed. A subagent
redacts as its parent does.

`abhed secret set` refuses a value under 8 characters, which would also match
ordinary text. A shorter value stored before that rule is still redacted, but
not in JSON object keys, so a value such as `type` cannot break the structure
of an event.

Redaction matches the exact value and its JSON-escaped and HTML-escaped
forms. For a value of 12 characters or more it also matches the forms a
command or client encodes it in: base64 and base64url at any byte alignment
(as in a Basic auth header that `curl -v` prints), hex in either case,
percent-encoding, and the whole value in lower or upper case. The same forms
are looked for in what `web_fetch` and `web_search` would send. All values
are replaced in one pass, longest first, so a label already written is never
matched again by a shorter value. Plain text (the sandbox control plane's
record, the prompt history) is redacted as text. An error a run returns, such
as a provider's error body, is redacted too, and a request to the model that
cannot connect reports its URL without the query or user information, where
a `base_url` may carry a key (`?api_key=`, `user:key@host`).

What redaction does not catch:

- A value split, reversed, partly sent, or encoded in another way (gzip,
  base32, a custom cipher) is not caught. No redactor can know every
  encoding, so give a secret only to the command that needs it, keep the
  [egress allowlist](21-network-policy.md) on, and rotate a value that was
  exposed.
- A value of 8 to 11 characters is matched only as written and escaped.
- Part of a value is not caught, such as a truncated one.
- Files are not redacted. What the agent writes to a file stays there.
  `abhed resolve` checks the change before it pushes: a diff, file name or
  commit message that holds a stored value, in any of the forms above,
  refuses the push and the pull request, and the change stays on its local
  branch.
- The model's own text and calls go back to the same model unredacted in the
  conversation. They are redacted everywhere else.

