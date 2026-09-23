# Security scans

> This record predates the split of the repository into a Community Edition
> and an Enterprise Edition. The commands and paths below (`deploy/`, the
> release bundle, the site checks) are quoted as they were run at the time;
> some of them now live in the Enterprise repository.

Independent, third-party tooling run against this repository and its
container image on 2026-09-14. Every finding below is listed with its
severity, its `file:line`, and a one-line triage — true positive, false
positive, accepted with reason, or to fix. Nothing here was fixed as part of
this pass; fixes are listed, most severe first, in
[Recommended fixes](#recommended-fixes) at the end.

This document is a point-in-time scan result, not a certification. Re-run it
before any release that changes dependencies, the Dockerfile, or the code
under `internal/`.

**Methodology note on the working tree scanned:** this repository had other,
unrelated work in progress in the same working directory for part of this
scan window — commits landed on `public-deploy` during the session (HEAD
moved from `de52025` to `44fe0da`), and a `bench/` benchmarking harness had
uncommitted changes throughout. None of that activity is this document's
concern and none of it was touched, reverted, or waited on. The source-level
tool runs (gosec, staticcheck, go vet, semgrep) reflect whatever the working
tree held at the moment each command ran, which is the correct and only
honest thing to scan — "the repository as it stood when the command ran" —
but it means a `file:line` citation here should be treated as accurate as
of that moment, not necessarily as of `HEAD` at the time you read this.
Where a scan disagreed with itself about a line number across tools run at
different times (see the semgrep `server.go` line-attribution note in
§3), that is the likely explanation. Re-running any tool against the current
`HEAD` is cheap and is how to get a citation that matches today's code
exactly.

## Tool versions and exact commands

| Tool | Version | Command |
|---|---|---|
| Go | go1.26.0 darwin/arm64 (ambient toolchain; `go.mod` pins `go 1.26.0`) | `env -u GOROOT go run golang.org/x/vuln/cmd/govulncheck@latest ./...` |
| govulncheck | v1.8.0 (DB downloaded 2026-09-14) | as above, plus a binary-mode cross-check: `podman run --rm -v <out>:/out --entrypoint /bin/sh localhost/abhed:local -c "cp /usr/local/bin/abhed /out/"`, then `env -u GOROOT go version -m abhed` (reports the image's actual go1.26.8) and `env -u GOROOT go run golang.org/x/vuln/cmd/govulncheck@latest -mode binary abhed` |
| gosec | v2.29.0 | `env -u GOROOT go run github.com/securego/gosec/v2/cmd/gosec@latest ./...` |
| staticcheck | 2026.2.1 (0.8.1) | `env -u GOROOT go run honnef.co/go/tools/cmd/staticcheck@latest ./...` |
| go vet | go1.26.0 | `env -u GOROOT go vet ./...` |
| semgrep | 1.177.0 | `semgrep --config auto --exclude web/zybuu --exclude docs --exclude internal/docsite/site --exclude node_modules --exclude .abhed-workspace .` |
| trivy | 0.74.0 (DB 2026-09-13) | `trivy image abhed:local` (via a `podman save` tarball — see note below) and `trivy fs --scanners vuln,secret,misconfig /Users/yuvrajsingh/titan` |
| gitleaks | 8.30.1 | `gitleaks detect --source /Users/yuvrajsingh/titan` |
| podman | 5.2.2 | `podman inspect abhed`, `podman exec abhed id`, `podman exec abhed cat /proc/1/status` |

**Note on `trivy image`:** `trivy image abhed:local` initially failed because
trivy tried to pull `abhed:local` from Docker Hub rather than reading it from
podman's local store. It was run instead against `podman save abhed:local -o
abhed-local.tar` followed by `trivy image --input abhed-local.tar`, which is
byte-for-byte the same image.

Scans that produce structured output were also captured as JSON
(`gosec.json`, `semgrep.json`, `gitleaks.json`) for tooling; this document is
built from those plus the plain-text logs.

---

**Lint, 15 September 2026.** golangci-lint v2.13.2 with the configuration in
`.golangci.yml` (the standard set plus errorlint, misspell, unconvert,
unparam, gocritic, bodyclose and nilerr) reported 181 findings on the
Community tree after the split, 109 of them in tests. All were resolved:
dead code removed (an unused compaction estimator, an unused execute path,
unused fields), constant parameters dropped, wrapped-error comparisons
corrected, response bodies closed on every path, and the agent loop now
keeps the first failure to write an event and ends the run at the next turn
boundary rather than continuing past a record that stopped. Errors that
carry no decision (writes to a terminal or an HTTP response, closes of
things only read from) are excluded by name in the configuration rather
than discarded one by one; a swallowed error that is intentional carries a
`nolint` directive with its reason beside it. The linter runs in CI and
blocks on any new finding.

## 1. govulncheck — known vulnerabilities in Go deps and stdlib as used

**Command:** `env -u GOROOT go run golang.org/x/vuln/cmd/govulncheck@latest ./...`

**Counts:**

| Bucket | Count | Meaning |
|---|---|---|
| Called (symbol-reachable) — stdlib | **19** | Your code actually calls a vulnerable stdlib function |
| Imported but not called — stdlib | 10 | The vulnerable package is in the import graph but govulncheck found no call path to the vulnerable symbol |
| Required but not called — modules | 5 | A vulnerable module is a dependency but nothing in it is reachable |

**Why govulncheck reports these at all, and why the currently-running image is likely already clean:**
this source-mode scan ran with the ambient local toolchain, `go1.26.0 darwin/arm64`
(`go.mod` pins `go 1.26.0`, and `GOTOOLCHAIN=auto` does not fetch a newer
patch release just to satisfy that directive). Every one of the 19 "called"
findings is fixed somewhere between go1.26.1 and go1.26.6, so against a
`go1.26.0` toolchain all 19 are real.

**However, the actual `abhed:local` image was not built with go1.26.0.**
The Dockerfile's build stage uses the floating tag `FROM golang:1.26-bookworm`,
which resolves to whatever the latest `1.26.x` point release is on the day
of `podman build` — not a version pinned in this repository. Extracting the
binary from the already-built image (`podman run ... cat /usr/local/bin/abhed`)
and inspecting its build info directly (`go version -m abhed`) shows it was
compiled with **go1.26.8**, five patch releases ahead of what `go.mod`
declares. Re-running govulncheck in `-mode binary` against that extracted
binary confirms **zero of the 19 stdlib vulnerabilities are present in the
deployed artifact** — only the unreachable `x/crypto/openpgp` advisory
remains (see below). In short: the currently-running container is very
likely already safe from all 19, but this is incidental to an unpinned base
image tag, not a property anyone verified or can rely on for the *next*
build. A future `podman build` on a day when `golang:1.26-bookworm` has not
moved forward, or a build pinned explicitly to `go1.26.0`, would reintroduce
all 19. This is itself the finding: **the safety here is accidental and
unpinned**, which is worse for a security review than either "vulnerable"
or "fixed" on their own — it cannot be relied upon to still be true at the
next rebuild.

None of the 19 are third-party dependency bugs; they are stdlib bugs fixed
upstream that `go.mod`'s declared `go 1.26.0` has not picked up, and that
the Dockerfile's floating tag happens to have picked up anyway, today, by
accident of when the image was last built.

### 19 called stdlib vulnerabilities

| Severity* | ID | Package | Fixed in | Reached from | Triage |
|---|---|---|---|---|---|
| Medium | GO-2026-6218 | `net/url` | go1.26.6 | `internal/k8s/client.go:400` (`http.Client.Do` → `url.Parse`) | to fix — trivially fixed by a toolchain bump |
| Medium | GO-2026-6090 | `crypto/tls` | go1.26.6 | `internal/server/server.go:1448` (`ListenAndServe`), `internal/k8s/client.go:406`, `internal/eval/eval.go:407` | to fix — toolchain bump |
| Medium | GO-2026-6089 | `net/http` | go1.26.6 | `internal/server/server.go:1448` | to fix — toolchain bump |
| Medium | GO-2026-6088 | `encoding/xml` | go1.26.6 | `internal/tools/document.go:148`, `internal/store/postgres.go:319` | to fix — toolchain bump |
| Medium | GO-2026-5972 | `encoding/asn1` | go1.26.6 | `internal/k8s/client.go:198` (`x509.CertPool.AppendCertsFromPEM`) | to fix — toolchain bump |
| High | GO-2026-5856 | `crypto/tls` (ECH privacy leak) | go1.26.5 | `internal/server/server.go:1448`, `internal/k8s/client.go:406` | to fix — toolchain bump |
| Medium | GO-2026-5039 | `net/textproto` | go1.26.4 | `internal/k8s/client.go:406` (`io.ReadAll` → `ReadMIMEHeader`) | to fix — toolchain bump |
| Medium | GO-2026-5037 | `crypto/x509` (hostname parsing perf) | go1.26.4 | `internal/server/server.go:1448` | to fix — toolchain bump |
| Medium | GO-2026-5026 | `golang.org/x/net/idna` via `net/http` | go1.26.6 | `internal/mcp/http.go:339`, `internal/k8s/client.go:400` | to fix — toolchain bump |
| Low | GO-2026-4971 | `net` (Windows NUL-byte panic) | go1.26.3 | `internal/remote/ssh.go:95/187`, `internal/server/server.go:1448`, `internal/store/postgres.go:92` | accepted with reason — Abhed's server and container run on Linux/macOS, not Windows; still fixed for free by the bump |
| High | GO-2026-4947 | `crypto/x509` (unbounded chain-building work) | go1.26.2 | `internal/server/server.go:1448` | to fix — toolchain bump; this is a DoS vector against the TLS listener |
| Medium | GO-2026-4946 | `crypto/x509` (inefficient policy validation) | go1.26.2 | `internal/server/server.go:1448` | to fix — toolchain bump |
| Medium | GO-2026-4918 | `net/http/internal/http2` (infinite loop on bad SETTINGS frame) | go1.26.3 | `internal/mcp/http.go:339`, `internal/k8s/client.go:400` | to fix — toolchain bump; DoS vector |
| High | GO-2026-4870 | `crypto/tls` (unauthenticated KeyUpdate → connection retention DoS) | go1.26.2 | `internal/server/server.go:1448`, `internal/k8s/client.go:406`, `internal/eval/eval.go:407` | to fix — toolchain bump; DoS vector against the public listener |
| Medium | GO-2026-5039 (dup listed above) | — | — | — | — |
| High | GO-2026-4602 | `os` (FileInfo escape from a Root) | go1.26.1 | `internal/eval/eval.go:95` (`os.ReadDir`) | to fix — toolchain bump |
| Medium | GO-2026-4601 | `net/url` (IPv6 host literal parsing) | go1.26.1 | `internal/auth/login.go:499`, `internal/server/server.go:1448`, `internal/k8s/client.go:400` | to fix — toolchain bump |
| High | GO-2026-4600 | `crypto/x509` (panic on malformed cert name constraints) | go1.26.1 | `internal/server/server.go:1448` | to fix — toolchain bump; a malformed client cert could crash the process |
| High | GO-2026-4599 | `crypto/x509` (incorrect email constraint enforcement) | go1.26.1 | `internal/server/server.go:1448` | to fix — toolchain bump |

*Severity as reported by the Go vulnerability database's own classification where given; several (marked DoS vector above) are practically higher risk for Abhed specifically because `internal/server/server.go:1448` is the public HTTPS listener.

**Net assessment:** none of these are code bugs in Abhed. All 19 are removed
by rebuilding with a current go1.26.x point release — no source change
required. Three or four (the `crypto/tls`/`crypto/x509` DoS and
chain-building issues reachable from `server.Server.ListenAndServe`) are
worth prioritizing since they sit directly on the internet-facing listener.

### 10 imported-but-not-called stdlib vulnerabilities

| ID | Package | Fixed in | Triage |
|---|---|---|---|
| GO-2026-6091 | `html/template` (JS context tracking) | go1.26.6 | accepted with reason — govulncheck found no call path; also fixed by the same toolchain bump |
| GO-2026-5942 | `net`/`x/net/dns/dnsmessage` (SVCB/HTTPS RR panic) | go1.26.6 | accepted with reason — no call path found |
| GO-2026-5038 | `mime` (quadratic `WordDecoder.DecodeHeader`) | go1.26.4 | accepted with reason — no call path found |
| GO-2026-4982 | `html/template` (meta content URL escaping bypass, XSS) | go1.26.3 | accepted with reason — no call path found; **verify no future feature renders untrusted content through `html/template`'s meta-content path**, since this class of bug is exactly the one to worry about if templating is ever added to the console UI |
| GO-2026-4981 | `net` (long CNAME crash) | go1.26.3 | accepted with reason — no call path found |
| GO-2026-4980 | `html/template` (escaper bypass, XSS) | go1.26.3 | accepted with reason — no call path found; same caveat as GO-2026-4982 |
| GO-2026-4970 | `os` (root escape via symlink + trailing slash) | go1.26.5 | accepted with reason — no call path found |
| GO-2026-4865 | `html/template` (JS brace-depth tracking, XSS) | go1.26.2 | accepted with reason — no call path found; same caveat |
| GO-2026-4864 | `internal/syscall/unix` (TOCTOU root escape via `Root.Chmod`, Linux) | go1.26.2 | accepted with reason — no call path found |
| GO-2026-4603 | `html/template` (meta content attribute URLs unescaped) | go1.26.1 | accepted with reason — no call path found; same caveat |

All ten disappear with the same toolchain bump as the called findings, at no
cost, so there is no reason to leave them un-fixed even though govulncheck
did not find a live call path today.

### 5 required-but-not-called module vulnerabilities

| ID | Module | Fixed in | Triage |
|---|---|---|---|
| GO-2026-5932 | `golang.org/x/crypto/openpgp` | N/A — package is unmaintained upstream | accepted with reason — `golang.org/x/crypto` is a direct dependency (`go.mod`), but nothing in Abhed imports the `openpgp` subpackage; govulncheck confirms no call path. No action possible short of dropping `golang.org/x/crypto` entirely, which is not realistic (it's also required for `x/crypto/ssh` used by `internal/remote`) |
| GO-2026-4986 | stdlib `net/mail` (quadratic string concat in `consumeComment`) | go1.26.3 | to fix — toolchain bump |
| GO-2026-4977 | stdlib `net/mail` (quadratic string concat in `consumePhrase`) | go1.26.3 | to fix — toolchain bump |
| GO-2026-4976 | stdlib `net/http/httputil` (ReverseProxy forwards oversized query) | go1.26.3 | to fix — toolchain bump; Abhed does not appear to use `httputil.ReverseProxy` today, but the fix is free |
| GO-2026-4869 | stdlib `archive/tar` (unbounded allocation, old GNU sparse format) | go1.26.2 | to fix — toolchain bump |

**govulncheck total: 34 distinct advisories, 0 requiring a code change, 34
resolved by explicitly pinning the Go toolchain (both `Dockerfile` and
`go.mod`) to go1.26.6 or later — see the pinning note above; the
currently-built image already happens to carry go1.26.8, verified by
extracting `/usr/local/bin/abhed` from `abhed:local` and running
`env -u GOROOT go version -m abhed` (reports `go1.26.8`) followed by
`env -u GOROOT go run golang.org/x/vuln/cmd/govulncheck@latest -mode binary abhed`,
which finds 0 of the 19 stdlib advisories in that binary — only the
`x/crypto/openpgp` module finding remains, unchanged from the source-mode
result and still not called by any Abhed code. This is a point-in-time
fact about today's image, not a guarantee about the next one; see
Recommended Fixes #1.**

---

## 2. gosec, staticcheck, go vet

### go vet

**Command:** `env -u GOROOT go vet ./...`

**Result: clean. 0 findings.**

### staticcheck

**Command:** `env -u GOROOT go run honnef.co/go/tools/cmd/staticcheck@latest ./...`

**Counts: 15 findings, all informational/style (staticcheck has no severity levels; classified here as LOW for consistency with the rest of this document).**

| Severity | Check | file:line | Finding | Triage |
|---|---|---|---|---|
| Low | U1000 | internal/agent/compact.go:55 | `(*Compactor).tokensPerTurn` is unused | to fix — dead code, safe to remove or wire in |
| Low | U1000 | internal/agent/loop.go:502 | `(*Loop).execute` is unused | to fix — dead code |
| Low | U1000 | internal/agent/subagent.go:28 | field `mu` is unused | to fix — dead field, likely a leftover from a refactor |
| Low | U1000 | internal/agent/subagent.go:29 | field `exhausted` is unused | to fix — dead field |
| Low | U1000 | internal/auth/login_test.go:24 | field `issuedFor` is unused | accepted with reason — test-only struct field, harmless |
| Low | SA1019 | internal/auth/oidc.go:235 | `(crypto/ecdsa.PublicKey).X` is deprecated since Go 1.26 | to fix — migrate to `PublicKey.Bytes`/`ParseUncompressedPublicKey` per the deprecation notice; this is cryptographic code, worth doing before the accessor is removed |
| Low | SA1019 | internal/auth/oidc.go:236 | `(crypto/ecdsa.PublicKey).Y` is deprecated since Go 1.26 | to fix — same migration, same call site |
| Low | U1000 | internal/jsonschema/schema.go:26 | field `rawOK` is unused | to fix — dead field |
| Low | SA1012 | internal/k8s/tool_test.go:108 | passes a nil `Context` where `context.TODO()` is idiomatic | accepted with reason — test code, functionally identical, purely a style nit |
| Low | SA4017 | internal/mcp/http.go:190 | `HasPrefix` result ignored (no side effects) | False positive — verified by reading the source: the call is the condition of a `switch` `case` (`case strings.HasPrefix(line, ":")`), not a discarded statement. Its return value is consumed by the `switch`; staticcheck's message ("return value is ignored") is misleading for this construct, not indicative of a bug. |
| Low | ST1005 | internal/model/anthropic.go:552 | error string ends with punctuation | accepted with reason — style-only, Go convention nit |
| Low | SA1012 | internal/remote/ssh_test.go:40 | nil `Context` passed | accepted with reason — test code |
| Low | SA1012 | internal/remote/ssh_test.go:52 | nil `Context` passed | accepted with reason — test code |
| Low | U1000 | internal/sandbox/process.go:23 | field `profile` is unused | to fix — dead field |
| Low | S1016 | internal/server/server.go:1021 | should convert `req` via a type conversion instead of a struct literal | accepted with reason — pure style suggestion, no behavior difference |
| Low | U1000 | internal/ui/lineedit.go:213 | `terminalWidth` func is unused | to fix — dead code |

### gosec

**In CI.** The triaged findings below are the baseline in
`deploy/ci/gosec-baseline.json`, keyed by rule and file. The workflow runs
gosec at the version recorded here and fails only on a finding outside that
baseline (`deploy/ci/gosec-gate.py`), so a new finding blocks a merge while
the known set does not keep the job permanently red. A fix that removes a
finding, or a new one once it has been triaged here, is followed by
`--update`, which rewrites the baseline from the current report.


**Command:** `env -u GOROOT go run github.com/securego/gosec/v2/cmd/gosec@latest ./...` (also captured as JSON via `-fmt=json -out=gosec.json`)

**Counts: 138 findings.**

| Severity | Count |
|---|---|
| HIGH | 24 |
| MEDIUM | 53 |
| LOW | 61 |

| Rule | Count | What it checks |
|---|---|---|
| G104 | 61 | Unchecked error return |
| G304 | 24 | Potential file inclusion via variable |
| G204 | 15 | Subprocess launched with a variable |
| G115 | 8 | Integer overflow on type conversion |
| G703 | 7 | Path traversal (taint analysis) |
| G306 | 6 | File written with permissions looser than 0600 |
| G118 | 6 | Goroutine uses `context.Background`/`TODO` where a request context is available |
| G301 | 5 | Directory created with permissions looser than 0750 |
| G124 | 4 | Cookie missing Secure/HttpOnly/SameSite |
| G122 | 2 | Filesystem op inside a `Walk`/`WalkDir` callback is TOCTOU-prone |
| G203 | 1 | `template.HTML` used without escaping |
| G705 | 2 | XSS via a response write |
| G404, G402, G704, G106, G120, G302 | 1 each | Weak RNG, insecure TLS config, SSRF heuristic, insecure SSH host-key check, unbounded form parsing, loose file permission |

**Triage counts across all 138:**

| Triage | Count |
|---|---|
| False positive | 63 |
| Accepted with reason | 67 |
| To fix | 8 |

#### G304 — potential file inclusion via variable (24 findings)

Abhed is an agentic coding harness: the model is expected to name files to
read and write, so a "variable" reaching a file-path call is the product's
core function, not automatically a bug. The real question for each finding
is whether the variable is model/request-controlled and, if so, whether it
passed through `tools.Session.Resolve` (`internal/tools/session.go:157`) —
the function that makes a model-supplied path absolute and proves it stays
inside the configured workspace root(s) — or `Server.resolveInWorkspace`
(`internal/server/download.go:99`), its HTTP-handler equivalent, before use.

| Severity | file:line | Finding | Triage |
|---|---|---|---|
| Medium | cmd/abhed/main.go:709 | `os.ReadFile` on a path from the session's own undo-tracker | False positive — path list is produced internally from calls already resolved |
| Medium | cmd/abhed/main.go:737 | `os.ReadFile` over `agent.DiscoverMemoryFiles` results | False positive — fixed filenames (`ABHED.md`, `ABHED.local.md`) under the workspace root |
| Medium | internal/agent/parallel.go:251 | `os.ReadFile` on `<git-common-dir>/info/exclude` | False positive — `gitDir` comes from `git rev-parse --git-common-dir` on the operator's own workspace |
| Medium | internal/agent/parallel.go:256 | same `p` re-read | False positive — same reasoning |
| Medium | internal/agent/prompt.go:187 | `os.ReadFile` over `opts.MemoryFiles` | False positive — fixed filenames, no attacker-controlled component |
| Medium | internal/config/config.go:490 | `os.ReadFile` in `mergeFile` | False positive — called only with operator/managed config paths resolved at startup |
| Medium | internal/eval/eval.go:104 | `os.ReadFile` while listing an eval corpus dir | False positive — `dir` is an operator CLI argument to the dev/CI eval harness |
| Medium | internal/eval/eval.go:196 | `os.Stat` for a `file_absent` assertion | False positive — path from operator-authored eval-task JSON fixtures |
| Medium | internal/eval/eval.go:206 | `os.ReadFile` for a `file_contains` assertion | False positive — same fixture provenance |
| Medium | internal/index/index.go:165 | `os.ReadFile` while building the search index | False positive — path is the OS's own `WalkDir` output over the configured index root |
| Medium | internal/index/index.go:191 | `os.ReadFile` in `Index.Update(ctx, path)` | **To fix** — this exported function has no containment check of its own; it is safe today only because every current caller pre-resolves the path. Add an explicit root check inside `Update` rather than relying on caller discipline |
| Medium | internal/k8s/client.go:136 | `os.ReadFile` of a kubeconfig from `$KUBECONFIG`/`~/.kube/config` | False positive — environment/operator-controlled, same trust level as `kubectl` |
| Medium | internal/k8s/client.go:309 | `os.ReadFile` of a TLS client cert path from kubeconfig | False positive — operator's own kubeconfig |
| Medium | internal/k8s/client.go:317 | `os.ReadFile` of the matching client key path | False positive — same |
| Medium | internal/remote/ssh.go:102 | `os.ReadFile` of an SSH identity file from operator config | False positive — equivalent to `ssh -i <path>` trust |
| High | internal/server/download.go:66 | `os.Open` on a path from `resolveInWorkspace` | False positive — validated two calls up the chain (download.go:51); gosec cannot see across the call, but the check is present and covered by `security_test.go:398` |
| Medium | internal/skills/skills.go:187 | `os.ReadFile` of `<dir>/SKILL.md` while loading operator-installed skills | False positive — `root` is operator-configured |
| Medium | internal/skills/skills.go:212 | `os.ReadFile` of a skill's pipeline file | False positive — same |
| Medium | internal/tools/edit.go:89 | `os.ReadFile` on `s.Resolve(a.Path)`'s result | False positive — validated immediately above at line 54 |
| Medium | internal/tools/file.go:71 | `os.ReadFile` on `s.Resolve(a.Path)`'s result | False positive — validated immediately above at line 55 |
| Medium | internal/tools/search.go:317 | `os.ReadFile` while walking during grep | False positive — walk root is resolved before the walk starts |
| Medium | internal/tools/session.go:55 | `os.ReadFile` in `recordChange` | False positive — internal helper called only with pre-resolved paths |
| Medium | internal/tools/session.go:252 | `os.ReadFile` in `ChangedSinceRead` | False positive — same |

#### G204 — subprocess launched with a variable (15 findings)

| Severity | file:line | Finding | Triage |
|---|---|---|---|
| Low | internal/agent/parallel.go:193 | `exec.CommandContext(ctx, "git", ...)` | False positive — fixed binary, argv args, no shell |
| Low | internal/agent/prompt.go:220 | `exec.Command("git", "-C", dir, "rev-parse", ...)` | False positive — same |
| Low | internal/agent/prompt.go:227 | `exec.Command("git", "-C", dir, "status", ...)` | False positive — same |
| Medium | internal/extension/extension.go:193 | `exec.CommandContext(ctx, cfg.Command, cfg.Args...)` launching an extension host | Accepted with reason — `cfg.Command`/`Args` are operator extension config, argv-based, no shell; equivalent trust to installing a local MCP server or editor extension |
| Medium | internal/k8s/client.go:340 | `exec.CommandContext` running a kubeconfig exec-credential plugin | False positive — identical trust model to `kubectl`'s own exec-credential support |
| Medium | internal/mcp/client.go:258 | `exec.CommandContext` launching a configured stdio MCP server | Accepted with reason — same reasoning as extension.go:193 |
| Medium | internal/sandbox/container.go:73 | `exec.Command(path, "info", ...)` probing a discovered runtime binary | False positive — `path` from `exec.LookPath` over a fixed allowlist (docker/podman/nerdctl) |
| Medium | internal/sandbox/container.go:101 | `exec.Command(c.runtime, "info", ...)` checking for gVisor | False positive — same `c.runtime` provenance |
| High | internal/sandbox/container.go:195 | `exec.CommandContext(ctx, c.runtime, ..., "/bin/sh", "-c", command)` — the model's shell command reaching a shell inside the container sandbox | Accepted with reason — this is the sandbox's actual job; the Dockerfile's own top comment states the in-process path check "is not a boundary against an attacker who reaches the process" and that the container is what makes the boundary real. Isolation is enforced by the container profile (verified below in §6), not by refusing this call |
| High | internal/sandbox/process.go:157 | same pattern for the macOS `sandbox-exec` / Linux `bwrap` backends | Accepted with reason — same, isolation enforced by the seatbelt profile / bwrap namespace flags |
| Low | internal/sandbox/process.go:243 | `exec.CommandContext(ctx, "bash", "-c", command)` in the `None` (no-isolation) tier | Accepted with reason — tier is explicitly named and self-describes as `"NO ISOLATION — commands run directly on the host. Trusted repositories only."` The risk is disclosed by the tier's own `Describe()`, not hidden |
| Low | internal/forge/resolve.go:99, :134 | `exec.CommandContext(ctx, "git", ...)` pushing the resolver's branch and running the worktree commands | False positive — fixed binary, argv args, no shell; the branch and directory names are built by the package, and the token reaches git through `GIT_CONFIG_*` environment variables, never argv |
| High | server/pty.go:125 | `exec.CommandContext(ctx, "bash", "-c", req.Command)` — a line typed into the workbench terminal, when no `Sandbox` function is configured | Accepted with reason — the same fallback as bash.go:151, for the person at the keyboard rather than the model. The line has already passed the policy engine as a `bash` call (`ManualAuthorize`), is recorded, and normally runs through the sandbox's own command builder; the fallback is the same `Sandbox == nil` deployment concern flagged below |
| High | internal/tools/bash.go:151 | `exec.CommandContext(runCtx, "bash", "-c", a.Command)` — the fallback when no `Sandbox` function is configured at all | Accepted with reason for the exec call itself (same "the bash tool runs shell commands" design). **The real risk is upstream**: whether a deployment can reach production with `b.Sandbox == nil`. Flagged in Recommended fixes below |

#### G104 — unchecked errors (61 findings, all LOW)

**Pattern:** roughly 50 of the 61 are best-effort cleanup or informational
writes: deferred/explicit `Close()` on sockets, pipes, HTTP response bodies,
and SSH sessions during teardown (`internal/mcp/*.go`, `internal/remote/*.go`,
`internal/model/retry.go`, `internal/telemetry/otlp.go`,
`internal/ui/lineedit.go`); `json.NewEncoder(w).Encode(...)` on
already-committed HTTP responses, where a write failure only truncates what
the client already started receiving (`internal/auth/*.go`,
`internal/server/*.go`); `bcrypt.CompareHashAndPassword` called deliberately
as dummy work for username-enumeration timing safety
(`internal/auth/local.go:182-184`, intentionally discarded); and
`l.Recorder.Record(...)` calls across `internal/agent/loop.go` and
`subagent.go` where the returned error is a best-effort audit-event-append
failure — a drop affects transcript completeness, not access control. Two
findings are genuine, if minor, misses.

| Severity | file:line | Finding | Triage |
|---|---|---|---|
| Low | cmd/abhed/main.go:1221 | `os.WriteFile(jsonPath, ...)` ignored, then unconditionally prints "report written to %s" | **To fix** — a failed write is reported to the operator as success |
| Low | cmd/abhed/main.go:1305 | `fs.Parse(rest)` return ignored in `abhed user add` flag parsing | Accepted with reason — malformed flags fail more specifically downstream |
| Low | internal/agent/export.go:100 | `json.Unmarshal` ignored rendering a session export | Accepted with reason — best-effort HTML render, leaves stats zero-valued |
| Low | internal/agent/loop.go:125,203,213-215,253-255,267,285,364-365,392-393,398-399,472-474,551-553,569-571,590-592,651-658,896 | `l.Recorder.Record(...)` ignored (15 call sites) | Accepted with reason — best-effort audit-log append; `Record`'s error is a store-append failure, not a policy decision |
| Low | internal/agent/subagent.go:270-275,293-300 | `rec.Record(...)` ignored (2 call sites) | Accepted with reason — same audit-log pattern |
| Low | internal/auth/local.go:182-184 | `bcrypt.CompareHashAndPassword` result discarded | False positive — deliberate timing-safety no-op, documented in the adjacent comment |
| Low | internal/auth/local.go:332 | `json.NewEncoder(w).Encode` ignored | Accepted with reason — response already committed |
| Low | internal/auth/login.go:408,411-418 | `json.NewEncoder(w).Encode` ignored (2 sites) | Accepted with reason — same |
| Low | internal/auth/middleware.go:155,174-175 | `json.NewEncoder(w).Encode` ignored (2 sites) | Accepted with reason — same |
| Low | internal/eval/eval.go:254 | `json.Unmarshal` ignored reading a recorded tool call | Accepted with reason — dev/CI eval harness reading its own fixtures |
| Low | internal/mcp/client.go:301 | `t.cmd.Process.Kill()` ignored | Accepted with reason — best-effort teardown |
| Low | internal/mcp/gateway.go:117,178 | `client.Close()`/`c.Close()` ignored (2 sites) | Accepted with reason — cleanup |
| Low | internal/mcp/http.go:129,280,284,302 | `resp.Body.Close()` ignored (4 sites) | Accepted with reason — cleanup |
| Low | internal/model/retry.go:166,197 | `resp.Body.Close()` ignored (2 sites) | Accepted with reason — cleanup |
| Low | internal/model/watsonx.go:131 | `json.Unmarshal(body, &out)` ignored | False positive — the very next line checks `out.AccessToken == ""`, which catches a parse failure |
| Low | internal/remote/ssh.go:173,199,268 | `Close()`/`Signal()` ignored (3 sites) | Accepted with reason — cleanup / best-effort kill signal |
| Low | internal/remote/tool.go:65,279,283 | `h.Close()` ignored (3 sites) | Accepted with reason — cleanup |
| Low | internal/server/admin_page.go:24, internal/server/console.go:29, internal/server/server.go:1274,1293,1545,1574 | `w.Write`/`Encode`/`Shutdown` ignored (6 sites) | Accepted with reason — static HTML writes and best-effort shutdown trigger |
| Low | internal/telemetry/otlp.go:372 | `resp.Body.Close()` ignored | Accepted with reason — cleanup on a path that already logs failures |
| Low | internal/tools/document.go:118 | `rc.Close()` ignored closing a zip entry reader | Accepted with reason — cleanup |
| Low | internal/tools/file.go:254,258 | `tmp.Close()` ignored inside error-return branches | False positive — the real error is already captured and returned; `Close()` is secondary cleanup |
| Low | internal/ui/lineedit.go:139,140,161,163,177,178,180,181 | pipe/terminal Write/Close ignored (8 sites) | Accepted with reason — best-effort terminal passthrough cleanup |

#### G115 — integer overflow on conversion (8 findings, all HIGH by gosec default)

| file:line | Finding | Triage |
|---|---|---|
| cmd/abhed/main.go:1665, :1769 | `int32(cfg.Storage.MaxConns)` (2 call sites) | False positive — small operator-set config integer, no realistic overflow path |
| internal/agent/id.go:46-51 | `byte(ms >> N)` truncating a millisecond timestamp into an ID (6 call sites) | False positive — deliberate, documented bit-packing to build a sortable ID |

#### G703 in the console's file viewer (`server/workbench.go`, `resolve`)

gosec flags `os.Stat(real)` because `real` derives from a request parameter.
**False positive.** The stat is the last thing `resolve` does, after the path
has gone through `tools.Session.Resolve`, had its symlinks followed, and been
checked with `filepath.IsLocal` against the resolved workspace root, and after
`.git`, `node_modules` and `.abhed` have been refused. It only asks whether the
path is a directory so the policy check can be put correctly. The read itself
goes through `os.Root`, which refuses a path that escapes even if a link is
swapped in after the check. `TestWorkbenchRefusesPathsOutsideTheWorkspace`
covers `../`, an absolute path outside, and a symlinked file and directory.

#### Syntax check on edits — two findings (G204, G304)

| file | Rule | Triage |
|---|---|---|
| `internal/tools/syntax.go` (`parsesPython`, `pythonVersion`) | G204 — subprocess launched with a variable | **Accepted.** The program is the real file behind the `python3` on the path, resolved through its symlinks — which also bypasses any virtualenv — and refused if it lies inside the workspace or an extra root, which the agent can write (`TestTheRealInterpreterIsRun`, `TestAnInterpreterInsideTheWorkspaceIsNeverRun`, `TestADotDotDirectoryIsInsideTheWorkspace`, `TestAnInterpreterInAnExtraRootIsNeverRun`, with `TestTheFakeInterpreterIsAcceptedOutsideTheRoots` showing those can fail). `pythonCommand` runs it with `-I -S`, an empty environment, the temp directory as working directory and a bounded wait (`TestPythonRunsIsolated`): `-S` keeps `site`, and so `.pth` files, from loading, and `-I` keeps the environment, user site packages and the current directory out. The script is a constant that calls `compile()`, which parses and does not execute (`TestPythonIsCompiledNotRun`); the content goes in on standard input and the process is killed after five seconds. An interpreter outside the roots is trusted as the operator's own; one planted in a directory the sandbox can write outside the roots, such as its temp directory, and put first on the harness's own `PATH`, would not be caught — an unusual setup the operator controls |
| `internal/tools/file.go` (`Write.Run`) | G304 — file inclusion via variable | **False positive**, the same as this file's other G304 findings: the path has already been through `Session.Resolve`, which confines it to the workspace roots and refuses harness state; the read takes the file's content before an overwrite, to decide whether the change breaks its syntax |

#### HawkEYE — three findings from the session report (G203, G304, G705)

HawkEYE renders a session's record as a page, and that record contains tool
output, which is untrusted by definition. So these three were read as "could
injected content run in a reviewer's browser", not as boilerplate.

| file | Rule | Triage |
|---|---|---|
| `hawkeye/render.go` (`chart`) | G203 — `template.HTML` bypasses escaping | **False positive.** The function builds an SVG from integers and floats through `%d`/`%.1f` and from `commas()`, which formats an `int`. No string from the record reaches it. Everything else on the page goes through `html/template`'s contextual escaping |
| `server/capabilities.go` (`serveIDEVendor`) | G705 — XSS via response write | **False positive.** The bytes written are files compiled into the binary with `go:embed` (the editor and terminal components under `server/ide/vendor`), chosen by a path that is looked up in that embedded tree and never read from disk or from the request beyond the file name. They are served with an explicit content type and `nosniff`; `TestIDEVendorServesOnlyEmbeddedFiles` asks for a path outside the tree and fails if anything but 404 comes back |
| `server/server.go` (`hawkeyeSession`) | G705 — XSS via response write | **False positive.** The bytes written are `html/template` output. The handler also sends `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'`, so a future template mistake still cannot execute script or load anything. `TestHTMLEscapesHostileToolOutput` feeds the renderer `<script>` and `onerror` payloads in the session id, the prompt, the arguments and the tool output, and fails if any arrives unescaped |
| `app/main.go` (`hawkeyeCmd`) | G304 — file inclusion via variable | **Accepted.** `abhed hawkeye <file>` reads the events file the operator named on their own command line, with their own permissions. It is not model- or request-controlled |

#### G703 — path traversal via taint analysis (6 findings, all HIGH by gosec default)

| file:line | Finding | Triage |
|---|---|---|
| internal/auth/filestore.go:53,89,92 | Read/write/rename of the local-auth `users.json` (3 sites) | False positive — `f.path` is the fixed, operator-configured location set at startup, never request-derived |
| internal/k8s/client.go:136 | Kubeconfig read | False positive — same as the G304 finding at this line, env/operator-controlled |
| internal/server/upload.go:113 | `os.MkdirAll` for `<workspace>/uploads/<sessionID>` | False positive — `sessionID` is validated by `validSessionID()` before use |
| internal/server/upload.go:118 | `os.WriteFile` at `dir/safeUploadName(header.Filename)` | False positive — `safeUploadName` (upload.go:162) strips path separators, control characters, and leading dots, and appends a random suffix; verified it cannot produce a traversal segment |

#### G306 — WriteFile permissions looser than 0600 (6 findings, MEDIUM)

| file:line | Finding | Triage |
|---|---|---|
| cmd/abhed-bench/main.go:141 | Benchmark results written 0644 | Accepted with reason — dev/CI tool output, not sensitive |
| cmd/abhed/main.go:845 | Written 0644 | To fix (verify) — confirm this path never carries credentials/tokens before accepting; tighten to 0600 if unsure |
| cmd/abhed/main.go:1221 | Eval/report JSON written 0644 | Accepted with reason — report output meant to be read by other tooling, not credential-bearing |
| internal/agent/undo.go:122 | Undo-log snapshot written 0644 | **To fix** — snapshots can contain the full content of any file the agent touched, including ones holding secrets; tighten to 0600 to match the precedent already set by `FileUserStore` (`internal/auth/filestore.go:87`) |
| internal/config/config.go:662 | Config file written 0644 | To fix (verify) — if this path can persist secrets (API keys, a DSN with a password) it must be 0600; confirm and tighten |
| internal/eval/eval.go:142 | Eval fixture/output written 0644 | Accepted with reason — dev/CI harness, non-sensitive |

#### G301 — directory permissions looser than 0750 (5 findings, MEDIUM)

All five (`internal/agent/parallel.go:225,255`, `internal/config/config.go:655`,
`internal/eval/eval.go:139,447`) create directories at `0755` for a git
worktree, `.git/info`, a config directory, and eval output. Triage: **accepted
with reason** for all — the directory listing being world-readable is low
risk as long as the *files* inside are correctly permissioned, which is the
G306 finding above (the one that actually matters for the config-directory
case).

#### G118 — goroutine uses `context.Background`/`TODO` (6 findings, HIGH by gosec default)

All six are **false positives**: each is a deliberately detached background
task with its own independent timeout, and most carry an
explicit comment saying so — `internal/mcp/client.go:104`'s connection-lifetime
read loop, `internal/mcp/http.go:293`'s SSE reader ("a slow tool call [should
not be] aborted when the triggering request context ends"),
`internal/server/admin.go:399`'s best-effort revocation email ("a bounced
notice must not leave the account still working"),
`internal/server/server.go:1570`'s shutdown-trigger goroutine (which by
definition runs after the request/server context is already done), and
`internal/server/settings.go:267`'s reindex goroutine ("client disconnecting
must not abandon a half-built index").

The sixth is `server/server.go`'s node heartbeat. The goroutine's own lifetime
is bound to the turn — it returns on `ctx.Done()` — and only the individual
claim refresh is detached, with a five-second timeout. It has to be: at the end
of a turn the run's context is already cancelled, so refreshing with it would
fail at exactly the moment the node is still alive and holding the session,
which is the failure the heartbeat exists to prevent.
`TestHeartbeatGoroutineDoesNotLeak` asserts the goroutine count is unchanged
across fifty heartbeats, so "detached write" does not quietly become
"leaked goroutine".

#### G124 — cookie missing Secure/HttpOnly/SameSite (4 findings, MEDIUM)

| file:line | Finding | Triage |
|---|---|---|
| internal/auth/local.go:225 | Sign-in cookie: `HttpOnly: true`, `SameSite: Lax` hardcoded; `Secure: l.Secure` is config-driven | **To fix** — `CookieSecure` (`internal/config/config.go:196`) is a plain Go `bool` that **defaults to `false`** unless an operator explicitly sets `"cookie_secure": true`. The comment above it even says "should be true anywhere but local HTTP development" — meaning the safe value is opt-in, not the default. A deployment that omits this setting serves session cookies over plain HTTP with no warning |
| internal/auth/local.go:283 | Sign-out cookie, same `Secure` wiring, no explicit `SameSite` | Accepted with reason — this cookie only clears the session (`MaxAge: -1`); missing SameSite here is low-value since there's no session value to protect. Same underlying `CookieSecure` gap as local.go:225, not a distinct issue |
| internal/auth/login.go:283 | Sign-in cookie (OIDC path), same pattern | **To fix** — same `CookieSecure` default-false gap |
| internal/auth/login.go:349 | Sign-out cookie (OIDC path), no explicit `SameSite` | Accepted with reason — same reasoning as local.go:283 |

#### G122 — filesystem op in Walk/WalkDir callback is TOCTOU-prone (2 findings, HIGH by gosec default)

`internal/index/index.go:165` and `internal/tools/search.go:317` both read a
file mid-walk without `os.Root`-scoped APIs. Triage: **accepted with reason**
for both — a TOCTOU race here requires local filesystem race access to a
directory the operator (index.go) or the resolved session workspace
(search.go) already controls; closing it with `os.Root` is a reasonable
future hardening step but not urgent.

#### Single-instance rules

| Rule | Severity | file:line | Finding | Triage |
|---|---|---|---|---|
| G404 | High | internal/model/retry.go:66 | `math/rand` used for retry-backoff jitter | False positive — not security-sensitive; `internal/agent/id.go` and `internal/server/upload.go` correctly use `crypto/rand` where randomness matters |
| G402 | High | internal/k8s/client.go:290 | `InsecureSkipVerify: true` in `OpenDirect` | Accepted with reason — deliberate and documented: this path is for a runtime-supplied server+token with no CA bundle available, analogous to `oc login --insecure-skip-tls-verify`; not the default connection path (`Open` honors the kubeconfig's own CA) |
| G704 | High | internal/remote/ssh.go:95 | "SSRF via taint analysis" on dialing `$SSH_AUTH_SOCK` | False positive — dials the local SSH agent socket named by an environment variable under operator control, not a request-reachable network destination |
| G106 | Medium | internal/remote/ssh.go:143 | `ssh.InsecureIgnoreHostKey()` | Accepted with reason — gated behind an explicit, named opt-in flag (`InsecureSkipHostKeyCheck`) with a comment acknowledging the MITM tradeoff; default behavior verifies `known_hosts` |
| G120 | Medium | internal/server/upload.go:87 | `ParseMultipartForm(8<<20)` flagged as unbounded | False positive — the handler already wraps the body in `http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))` two lines above |
| G302 | Medium | internal/agent/parallel.go:256 | `os.OpenFile(..., 0o644)` for `.git/info/exclude` | Accepted with reason — git's own internal per-worktree file, conventionally world-readable like the rest of `.git` |

---

## 3. semgrep (`--config auto`)

**Command:** `semgrep --config auto --exclude web/zybuu --exclude docs --exclude internal/docsite/site --exclude node_modules --exclude .abhed-workspace .`

**Counts: 27 findings — 9 ERROR, 18 WARNING (semgrep's own severity labels; "Ran 551 rules on 187 files").**

| Severity | file:line | Rule | Triage |
|---|---|---|---|
| Warning | deploy/sitetests/demo.test.mjs:19 | hardcoded-hmac-key | False positive — `SECRET = "test-secret-not-the-real-one"`, a named test fixture for `deploy/send-demo.sh`'s contract tests, not a real secret |
| Warning | deploy/sitetests/demo.test.mjs:40 | hardcoded-hmac-key | False positive — same test file, a different signature computed with the string `"other"` to test the wrong-signature rejection path |
| Error | internal/auth/local.go:183 | detected-bcrypt-hash | False positive — the dummy hash string is deliberate, documented, constant-time-comparison filler for username-enumeration defense, not a leaked real credential |
| Warning | internal/auth/local.go:225 | cookie-missing-secure | Same finding as gosec G124 above — to fix (CookieSecure defaults false) |
| Warning | internal/auth/local.go:283 | cookie-missing-secure | Same as gosec G124 — accepted with reason (logout cookie) |
| Warning | internal/auth/login.go:283 | cookie-missing-secure | Same as gosec G124 — to fix |
| Warning | internal/auth/login.go:292 | open-redirect | False positive — `pend.Return` is validated by `safeReturn()` (login.go:436, requires a same-origin `/...` path, rejects `//`) at the point it is stored (login.go:206), before this redirect ever reads it |
| Warning | internal/auth/login.go:349 | cookie-missing-secure | Same as gosec G124 — accepted with reason (logout cookie) |
| Warning | internal/auth/login.go:463 | no-fprintf-to-responsewriter | False positive — verified by reading `loginError`: both interpolated values (`kind`, `detail`, sourced from attacker-controlled OIDC `error`/`error_description` query parameters, per the function's own comment) are passed through `template.HTMLEscapeString` before the `Fprintf`. The comment shows this was a deliberate, considered mitigation, not an oversight. |
| Warning | internal/auth/middleware.go:71 | open-redirect | False positive — the `Location` header target is the fixed literal `/`; the request path is only appended as a `?next=` query value via `url.QueryEscape`, never used to build the redirect target itself |
| Warning | internal/auth/middleware.go:92 | open-redirect | False positive — same pattern, fixed `/login` target with an escaped `?return=` query value |
| Error | internal/extension/extension.go:193 | dangerous-exec-command | Same finding as gosec G204 — accepted with reason (operator extension config, argv-based) |
| Warning | internal/k8s/client.go:289 | bypass-tls-verification | Same finding as gosec G402 — accepted with reason (documented `OpenDirect` tradeoff) |
| Error | internal/k8s/client.go:340 | dangerous-exec-command | Same as gosec G204 — false positive (kubeconfig exec-credential plugin, same trust as `kubectl`) |
| Error | internal/mcp/client.go:258 | dangerous-exec-command | Same as gosec G204 — accepted with reason (configured MCP server) |
| Warning | internal/model/retry.go:7 | math-random-used | Same as gosec G404 — false positive (retry jitter only) |
| Warning | internal/remote/ssh.go:143 | avoid-ssh-insecure-ignore-host-key | Same as gosec G106 — accepted with reason (explicit opt-in flag) |
| Error | internal/sandbox/container.go:73 | dangerous-exec-command | Same as gosec G204 — false positive (fixed runtime binary from LookPath) |
| Error | internal/sandbox/container.go:101 | dangerous-exec-command | Same as gosec G204 — false positive |
| Error | internal/sandbox/container.go:195 | dangerous-exec-command | Same as gosec G204 — accepted with reason (the sandbox's actual job) |
| Error | internal/sandbox/process.go:243 | dangerous-exec-command | Same as gosec G204 — accepted with reason (`None` tier, self-describing as unsafe) |
| Warning | internal/server/admin_page.go:24 | no-direct-write-to-responsewriter | Accepted with reason — writes a fixed, source-controlled HTML constant (`adminHTML`), no user/model data interpolated |
| Warning | internal/server/console.go:29 | no-direct-write-to-responsewriter | Accepted with reason — same, fixed `consoleHTML` constant |
| Warning | internal/server/server.go:1145 | no-direct-write-to-responsewriter | Tool artifact, not a real finding at this location — verified by reading both the reported line and semgrep's own byte offset (40821): it lands inside a `select { case live.approvals <- ... }` statement in `approveAction`, nowhere near a `ResponseWriter` write. See note below. |
| Warning | internal/server/server.go:1164 | no-direct-write-to-responsewriter | Tool artifact, not a real finding at this location — byte offset (41702) lands in a doc comment above `func (s *Server) signOut`, not a write call. See note below. |
| Warning | internal/server/server.go:1410 | no-fprintf-to-responsewriter | Tool artifact, not a real finding at this location — byte offset (50797) lands inside an unrelated `if local := s.localAuth(); local != nil { ... }` block. See note below. |
| Error | internal/tools/bash.go:151 | dangerous-exec-command | Same as gosec G204 — accepted with reason for the exec call; the real risk is the `Sandbox == nil` fallback path, tracked in Recommended fixes |

**Net: 27 findings, 0 exploitable, 0 true positives.** Every one was checked against source, including the 4 that this triage initially flagged for follow-up verification; that follow-up is done and closes them as false positive/accepted with reason. Three of those four (`server.go:1145`, `:1164`, `:1410`) turned out to be a line-number attribution issue in this semgrep run: cross-checking the JSON result's own byte offsets against the current file shows they do not land on any `w.Write`/`fmt.Fprintf` call at all — offset 40821 lands mid-`select` statement in `approveAction`, offset 41702 lands in a doc comment above `signOut`, and offset 50797 lands in an unrelated `if s.localAuth()...` block. The real `w.Write`/`fmt.Fprintf` call sites in `server.go` are a few lines away, at 1277 (`authDisabledHTML`, a fixed constant), 1296 (`landingHTML` via the same `withHome()` HTML-escaping helper used by `admin_page.go`/`console.go`, already accepted above), and 1542 (`writeSSE`, writing an integer sequence number plus JSON-marshaled event data — JSON escaping neutralizes HTML-significant characters for this use). All three are false positives once matched to their actual code; recorded rather than silently dropped in case a different semgrep version reproduces the mismatch differently. Everything else is either a documented, deliberate design choice (the sandbox/exec findings, the SSH/TLS opt-outs) or a genuine false positive (the bcrypt/HMAC "secrets" that are test fixtures, the open-redirect findings that are already gated, and the two now-closed `Fprintf`/`HasPrefix` items above).

---

## 4. trivy

### `trivy image abhed:local`

**Command:** `podman save abhed:local -o abhed-local.tar && trivy image --input abhed-local.tar` (see note in the tool-versions table on why the tarball route was needed)

**Base image:** `debian:bookworm-slim` (Debian 12.15) at build time, `golang:1.26-bookworm` build stage.

| Target | Total | Critical | High | Medium | Low | Unknown |
|---|---|---|---|---|---|---|
| OS packages (Debian) | 646 | 16 | 131 | 267 | 218 | 14 |
| Python packages | 41 | 0 | 2 | 36 | 3 | 0 |
| `usr/local/bin/abhed` (Go binary) | 1 | 0 | 0 | 0 | 0 | 1 |

**Why the OS count is large:** the runtime stage installs `ca-certificates`,
`git`, `ripgrep`, `curl`, `python3`/`python3-pip`, `zip`, `unzip`,
`graphviz`, and `bubblewrap` (Dockerfile, documented per-package). `graphviz`
and the `pip3 install matplotlib` step in particular pull in a large
transitive chain of image/codec libraries (`libaom3`, `libde265-0`,
`libheif`-adjacent packages, etc.) that a pure Go HTTP service would not
otherwise need — this is the direct cause of the CVE count being much higher
than the size of Abhed's own code would suggest.

**8 distinct CRITICAL-severity OS packages** (16 rows because `perl`,
`perl-base`, and `perl-modules-5.36` share one CVE):

| Package | CVE | Status | Triage |
|---|---|---|---|
| `libaom3` | CVE-2023-6879 (heap-buffer-overflow on frame size change) | affected | To fix — pulled in transitively by `matplotlib`/`graphviz`; Abhed never decodes AV1 video, so this is unreachable in practice, but it should not exist in the image at all. See Recommended fixes for the packaging-level fix |
| `libglib2.0-0` | CVE-2026-58016 (integer underflow in D-Bus introspection XML) | fix_deferred | Accepted with reason — no D-Bus usage in this image; upstream has deferred a fix |
| `libperl5.36` / `perl` / `perl-base` / `perl-modules-5.36` | CVE-2026-13221 (regex processing) | affected | To fix (packaging) — Perl is a transitive dependency of Debian's base tooling, not something Abhed invokes; remove or minimize if practical |
| `libsqlite3-0` | CVE-2025-7458 (integer overflow) | — | Accepted with reason — Abhed's own storage is Postgres or a JSON file (`internal/store`), not SQLite; this is base-image baggage |
| `zlib1g` | CVE-2023-45853 (integer overflow, heap buffer overflow) | will_not_fix | Accepted with reason — upstream has marked this will-not-fix; zlib is used transitively by many tools in the image |

**Assessment:** none of the 8 CRITICAL packages are exercised by Abhed's own
code paths (video codecs, D-Bus, Perl, SQLite are all incidental to the
apt/pip package set, not things `cmd/abhed` calls). They are real
vulnerabilities in the image an attacker who achieved code execution inside
the container could potentially pivot on, which is exactly the scenario the
container hardening in §6 is designed to contain — but they should not be
shipped if they can be trimmed. See Recommended fixes.

**Python packages — the one library that matters:** all 41 Python findings
are in **pypdf 5.1.0** (pinned in the Dockerfile), which has roughly 30 known
denial-of-service CVEs (crafted PDFs causing infinite loops, unbounded memory
allocation via LZW/RunLength/FlateDecode streams, malformed trailers, etc.),
fixed progressively up through pypdf 6.14.2. **This is not incidental
baggage** — `internal/tools/document.go`'s `extractPDFViaPython`
(`document.go:264`) shells out to `python3` running pypdf specifically to
parse PDFs the agent is given (uploaded files, per `internal/server/upload.go`),
so a malicious PDF is a real, reachable input to this exact vulnerable
library.

| Severity | Count | Triage |
|---|---|---|
| High | 2 | To fix — CVE-2026-59935/59936, DoS via crafted inline images; reachable from any PDF handed to the agent |
| Medium | 36 | To fix — the remaining DoS CVEs in the same library, same reachability |
| Low | 3 | Accepted with reason — lower-severity DoS variants |

**Go binary (`usr/local/bin/abhed`):** 1 UNKNOWN-severity finding —
`golang.org/x/crypto` GO-2026-5932, the same "openpgp is unmaintained"
advisory already covered under govulncheck §1 above (no call path to the
affected subpackage). Not double-counted as a new issue.

### `trivy fs --scanners vuln,secret,misconfig /Users/yuvrajsingh/titan`

**Result:**

| Target | Type | Vulnerabilities | Secrets | Misconfigurations |
|---|---|---|---|---|
| `.abhed-workspace/go.mod` | gomod | 0 | – | – |
| `go.mod` | gomod | 1 | – | – |
| `Dockerfile` | dockerfile | – | – | 0 (clean) |

The one `go.mod` finding is `golang.org/x/crypto` GO-2026-5932 (UNKNOWN
severity) — the same openpgp advisory as above, not a new issue.

**Secret scanning found 0 secrets in the working tree.** This is expected:
`deploy/.db-password`, `deploy/.db-admin-password`, and
`deploy/.demo-secret` are real, intentional secret files, but they are
`.gitignore`d and were present on disk during this scan — `trivy fs`
scans the working tree including untracked files, so their absence from the
report confirms trivy's secret detector did not flag their *contents*, only
that they exist as files trivy chose not to pattern-match (short, high
entropy but no recognizable "password:"-style key=value shape in the exact
format its rules look for). Their presence as gitignored, intentional
secret material is confirmed directly by listing them — contents are not
reproduced here or anywhere in this document.

**Misconfiguration scanning found 0 issues in the Dockerfile** — consistent
with the hardening choices already documented in Dockerfile's own comments
(non-root user, no baked-in host paths, minimal package set relative to its
job).

---

## 5. gitleaks — full git history

**Command:** `gitleaks detect --source /Users/yuvrajsingh/titan` (report saved as JSON with `-v`)

**Result: 1 finding, across 160 commits scanned (~2.94 MB).**

| Severity | Commit | file:line | Rule | Triage |
|---|---|---|---|---|
| Medium | 9aaf949abb0be375c0bd3d12130361627daa49d9 | docs/ops/enabling-auth.md:42 | generic-api-key | **False positive** — the matched text is `# generated password: 7Kq2mVx9pLd4  (change it after first sign-in)`, an illustrative example of CLI output in a documentation code block showing what `abhed user add` prints, not a real credential tied to any live system |

**The expected historical secret was not what gitleaks' pattern rules
caught, but it is confirmed present in history by direct commit
inspection:** `deploy/.db-password` was tracked in git until commit
`0cff60f3aa82d27b614db64341f0d302dee03328` ("Delete a chat from the console,
and stop connecting to Postgres as a superuser"), whose message states
directly: *"deploy/.db-password was tracked in git; it is untracked,
ignored, and the password rotated."* `git log --all --full-history` confirms
this is the only commit touching that path, and the password has since been
rotated per that commit's own message. Gitleaks' generic secret-pattern
rules did not flag the historical password value itself in this run (it may
not match any of gitleaks' built-in regexes for its format), but the fact
and the commit are independently confirmed by history rather than by
pattern-matching. **Recorded here as fact and commit hash only — the value
itself is not reproduced, consistent with it being rotated but still
git history that should not be casually re-surfaced.**

**Net: gitleaks found nothing that represents a live credential today.** The
one pattern match is documentation prose; the one real historical secret is
already rotated and confirmed by the commit trail rather than by a
pattern-match hit.

---

## 6. Container posture

**Commands:** `podman inspect abhed`, `podman exec abhed id`, `podman exec abhed cat /proc/1/status | grep -i cap`

| Property | Value | Assessment |
|---|---|---|
| User | `10001:10001` (config), confirmed at runtime: `uid=10001(abhed) gid=10001(abhed) groups=10001(abhed)` | Non-root, fixed UID that owns nothing in the image (Dockerfile) |
| Capabilities (inspect `CapDrop`) | `CAP_CHOWN, CAP_DAC_OVERRIDE, CAP_FOWNER, CAP_FSETID, CAP_KILL, CAP_NET_BIND_SERVICE, CAP_SETFCAP, CAP_SETGID, CAP_SETPCAP, CAP_SETUID, CAP_SYS_CHROOT` dropped, `CapAdd: []` | All Linux capabilities dropped, none added |
| Capabilities (runtime, `/proc/1/status`) | `CapInh/CapPrm/CapEff/CapBnd/CapAmb` all `0000000000000000` | Confirmed empty at runtime — matches the inspect-time configuration exactly |
| Read-only rootfs | `ReadonlyRootfs: true` | Confirmed |
| No-new-privileges | `SecurityOpt: ["no-new-privileges"]` | Confirmed |
| Privileged | `false` | Confirmed |
| Memory limit | 2 GiB (`Memory` and `MemorySwap` both 2147483648 — i.e. no swap beyond the memory limit) | Set |
| PIDs limit | 512 | Set |
| CPU limit | 2 CPUs (`NanoCpus: 2000000000`, `CpuQuota: 200000` at the default 100ms period) | Set |
| Published ports | `8080/tcp` bound to `127.0.0.1:8080` only | Loopback-only, not exposed on all interfaces |
| Network mode | `bridge` | Standard podman bridge, not host networking |
| Mounts | `abhed-workspace` volume → `/workspace` (rw, `nosuid,nodev`); `abhed-state` volume → `/home/abhed/.abhed` (rw, `nosuid,nodev`); `.abhed/skills` bind → `/workspace/.abhed/skills` (ro); `deploy/config.json` bind → `/etc/abhed/config.json` (ro) | Config and skills mounted read-only; the two writable mounts are both `nosuid,nodev` |

**Assessment: this is a well-hardened container configuration.** Non-root
UID with nothing in the image owned by it, every Linux capability dropped
and confirmed empty at the kernel level (not just requested), read-only
rootfs, no-new-privileges, resource limits on memory/PIDs/CPU, the HTTP port
bound to loopback rather than all interfaces, and both writable volumes
mounted `nosuid,nodev`. No findings here.

---

## 7. Repo's own hardening tests

**Command:** `env -u GOROOT go test ./internal/sandbox/ ./internal/deploycheck/ ./internal/sitecheck/ -count=1`

**Result: all pass. 0 failures.**

| Package | Tests | Result |
|---|---|---|
| `internal/sandbox` | `TestProcessSandboxBlocksWriteOutsideWorkspace`, `TestProcessSandboxBlocksSystemPathWrite`, `TestProcessSandboxBlocksNetworkByDefault`, `TestProcessSandboxBlocksCredentialRead`, `TestSandboxReportsItsOwnTier`, `TestSelectRefusesToDowngrade`, `TestSelectPrefersStrongest`, `TestNoneTierIsHonestAboutItself`, `TestTierOrdering`, `TestResourceLimitsRejectForkBomb`, `TestSandboxAllowsToolchainTempDir`, `TestSandboxAllowsGoBuild` | 12/12 PASS |
| `internal/deploycheck` | `TestDeployedDenyRulesMatchRealPaths`, `TestWebSearchSurvivesSandboxHardening` | 2/2 PASS |
| `internal/sitecheck` | `TestAttackCountOnPageMatchesSuite`, `TestFunctionCountOnPageMatchesRepo`, `TestPageClaimsNoInstallPathThatDoesNotExist`, `TestPageClaimsNoCertification`, `TestFormActionAgreesWithCSP`, `TestEventListMatchesItsOwnCount`, `TestPageDoesNotClaimUnenforcedControls`, `TestProviderCountOnPageMatchesRegistry`, `TestSDKDocDoesNotOverclaim`, `TestDurabilityClaimsNameTheDriver`, `TestPageDoesNotContradictItsOwnLimitations`, `TestInternalDocsCiteFilesThatExist` | 12/12 PASS |

These tests are notable because several of them (`TestSelectRefusesToDowngrade`,
`TestResourceLimitsRejectForkBomb`, `TestDeployedDenyRulesMatchRealPaths`) are
exactly the kind of check that would have caught some of the risk classes
this scan looked for by hand — e.g. `TestDeployedDenyRulesMatchRealPaths`
independently asserts the deployed policy denies SSH keys, cloud
credentials, `.env` files, and the Abhed config itself. Their passing is
evidence the repo tests its own security claims, not just a green CI badge.

---

## Recommended fixes

Ordered most severe first. None of these were applied as part of this scan.

1. **Pin the Go build image and `go.mod`'s toolchain directive to an
   explicit go1.26.x patch release, rather than relying on a floating tag.**
   `Dockerfile`'s `FROM golang:1.26-bookworm` and `go.mod`'s `go 1.26.0`
   currently disagree — the floating Dockerfile tag happened to resolve to
   go1.26.8 on the day this image was last built (confirmed by extracting
   the binary and running `go version -m` and `govulncheck -mode binary`
   against it: 0 of the 19 stdlib vulnerabilities are present in today's
   image), while `go.mod`'s pinned `1.26.0` is fully vulnerable to all 19.
   That is not a fix, it is luck that will not survive every future build.
   Change both to name the same explicit, current patch release (e.g.
   `FROM golang:1.26.8-bookworm` and `go 1.26.8` in `go.mod`), and re-pin
   both together as new patches ship, so the image's safety no longer
   depends on which day `docker/podman build` happened to run. This
   resolves all 34 govulncheck advisories (19 actively called, including
   several DoS and crash vectors reachable from the public HTTPS listener
   in `internal/server/server.go:1448` and the k8s client) at zero
   code-change cost, and makes the fix durable instead of incidental.
   Highest value, lowest effort item in this report.

2. **Upgrade or replace `pypdf` in the Dockerfile** (currently pinned at
   `5.1.0`; ~30 known DoS CVEs, 2 HIGH, fixed progressively through
   `6.14.2`). This library parses PDFs supplied by whoever can reach the
   upload endpoint or hand the agent a file — a genuinely reachable,
   internet-facing attack surface, not incidental image baggage. Bump the
   pin to a current `pypdf` release and re-run `trivy image` to confirm the
   CVE count drops to near zero for this package.

3. **Make `CookieSecure` default to safe** (`internal/config/config.go:196`,
   used by `internal/auth/local.go:225` and `internal/auth/login.go:283`).
   The field is a plain `bool` that defaults to `false` unless an operator
   explicitly sets `"cookie_secure": true` — meaning the safe configuration
   is opt-in. Either default it to `true` and require an explicit
   `false` for local HTTP development, or have `internal/deploycheck` (which
   already asserts other security-relevant config invariants) fail the
   build when auth is enabled, `cookie_secure` is false, and the bind
   address is not loopback.

4. **Tighten `internal/agent/undo.go:122`'s `os.WriteFile` from 0644 to
   0600.** Undo-log snapshots capture the full content of any file the agent
   has touched, which can include secrets; the same repo already sets 0600
   for exactly this reason in `internal/auth/filestore.go:87`. Verify and
   apply the same fix to `internal/config/config.go:662` and
   `cmd/abhed/main.go:845` if those paths can ever contain credential
   material.

5. **Give `internal/index/index.go:191`'s exported `Index.Update(ctx, path)`
   its own containment check** rather than relying on every current and
   future caller to have already resolved the path through
   `Session.Resolve`. Low cost, closes a latent trust-boundary gap that
   gosec correctly flagged even though no current caller exploits it.

6. **Low priority, verified safe but worth a defensive assertion:**
   `internal/tools/bash.go:151`'s `Sandbox == nil` fallback runs the agent's
   shell command directly on the host with no isolation at all when no
   sandbox function is configured. Traced all production wiring
   (`cmd/abhed/main.go:188,895,1138`, all three via `buildSandbox` at
   line 154, which calls `fail(err)` and exits on any error before
   `tools.Bash{Sandbox: sb.Command}` is ever constructed): the nil-sandbox
   branch is unreachable from the shipped CLI today, only reachable by a
   library caller outside `cmd/abhed` that constructs `tools.Bash{}`
   directly without setting `Sandbox`. Consider making `tools.Bash`'s
   zero-value refuse to run rather than silently falling back to
   unsandboxed `exec.Command`, so a future integration cannot reintroduce
   this by omission.

7. **Migrate `internal/auth/oidc.go:235-236` off the deprecated
   `ecdsa.PublicKey.X`/`.Y` accessors** (staticcheck SA1019) to
   `PublicKey.Bytes()`/`ParseUncompressedPublicKey` or
   `x509.MarshalPKIXPublicKey`/`ParsePKIXPublicKey`, per the Go 1.26
   deprecation notice. This is cryptographic code; worth doing before the
   accessors are removed in a future Go release, not just before they start
   producing warnings.

8. **Trim the Dockerfile's package set to reduce the OS CVE surface**
   (currently 646 CVEs across the Debian base, including 8 distinct CRITICAL
   packages: `libaom3`, `libglib2.0-0`, `perl`/`perl-base`/`perl-modules`,
   `libsqlite3-0`, `zlib1g`). None of these are directly exercised by
   Abhed's own code, but `graphviz` and `matplotlib` (installed for document
   generation) pull in a large transitive chain of image/codec libraries
   Abhed never uses. Consider whether `matplotlib`'s chart-generation use
   case can be served by a lighter plotting path, or accept the current
   trade-off explicitly in `docs/trust/security-posture.md` rather than
   leaving it implicit.

9. **Clean up dead code** flagged by staticcheck (U1000): unused
    functions/fields in `internal/agent/compact.go:55`, `loop.go:502`,
    `subagent.go:28-29`, `internal/jsonschema/schema.go:26`,
    `internal/sandbox/process.go:23`, `internal/ui/lineedit.go:213`, and
    `internal/server/admin.go:60` (`(*Server).isAdmin`, worth a specific
    check that no authorization path was meant to call this and silently
    doesn't). No security impact from the dead code itself; housekeeping.

10. **Style only, no urgency:** `internal/model/anthropic.go:552` (error
    string ends in punctuation, ST1005) and `internal/server/server.go:1021`
    (struct-literal-instead-of-conversion, S1016).

(`internal/mcp/http.go:190`'s SA4017 and the four semgrep raw-`ResponseWriter`-write
findings flagged for follow-up above have since been verified by reading the
source in each case and are closed as false positives — see §2 and §3 for
the specifics. No fix required.)

## Fixes applied — 14 September 2026

Applied the same day, in commit order after this report was written:

1. **Toolchain pinned.** `go.mod` now declares `go 1.26.8` and the Dockerfile
   builds `FROM golang:1.26.8-bookworm`; the two can no longer disagree, and
   the 34 govulncheck advisories against 1.26.0 do not apply to what is
   built. Re-pin both together when the next patch ships.
2. **pypdf upgraded** from 5.1.0 to 6.18.1 in the image.
3. **`cookie_secure` defaults to true** (`internal/config/config.go`,
   `Default()`); plain-HTTP development now has to ask for the insecure
   setting explicitly.
4. **Owner-only files**: undo snapshots (`internal/agent/undo.go`), the
   written config (`internal/config/config.go`) and `abhed init`'s output
   (`cmd/abhed/main.go`) are created 0600.
5. **`Index.Update` refuses paths outside its root**
   (`internal/index/index.go`), independent of what the caller resolved.
6. **OIDC EC keys** are parsed with `ecdsa.ParseUncompressedPublicKey`,
   which validates the point is on the curve; the deprecated `X`/`Y`
   construction is gone (`internal/auth/oidc.go`).
7. **Image surface**: the runtime stage applies Debian security upgrades
   and removes `python3-pip` after the document libraries are installed.

Not applied, on purpose: `tools.Bash{}` with no sandbox still runs on the
host. That is the SDK's documented contract — the embedding program owns
isolation, and the site and `sdk/abhed.go` say so — and a guard in
`internal/sitecheck` keeps the two statements consistent. Changing it is a
product decision about the SDK, not a scan finding.

### Image rescanned after the fixes

Same tool (trivy, vuln scanner) against the image as rebuilt and deployed on
14 September 2026, now on `debian:trixie-slim` with the pinned Go 1.26.8
toolchain, pypdf 6.18.1 and pip removed:

| Severity | Before | After |
|---|---|---|
| CRITICAL | 16 | 0 |
| HIGH | 129 | 71 |
| MEDIUM | 260 | 131 |
| LOW | 214 | 152 |
| pypdf findings | 41 | 0 |

What remains is the Debian base's own backlog in packages Abhed does not
call (graphics and codec libraries pulled in by graphviz and matplotlib for
the document tools). The next reduction is to move document generation out
of the runtime image into a separate, optional one; that is on the list,
not done. In-container checks after the rebuild: the document libraries
import, bubblewrap 0.12 runs a command under the process tier, and
`abhed doctor` reports the sandbox exec check as ok.
