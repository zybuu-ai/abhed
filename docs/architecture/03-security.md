# Abhed — Execution Isolation & Prompt-Injection Defense

Status: Draft · 2026-09-02

> ## ⚠ Read this first
>
> The research pass produced **zero verified claims** on sandboxing, and **two candidate
> sandboxing claims were refuted 0-3** by adversarial verification. The isolation posture
> of every comparable agent is therefore **unverified** in this evidence set. One notable
> data point that did survive: a major open-source agent SDK made sandboxing **opt-in
> rather than mandatory** in its V1 rewrite.
>
> **This was the single most dangerous gap in the Abhed plan.** Everything below remains
> [E] engineering judgment, but it is now **implemented and verified by escape tests**
> rather than asserted (`internal/sandbox`). A red-team engagement is still required
> before running genuinely hostile code — automated tests prove the controls work as
> designed, not that a determined attacker cannot defeat them.

## 1. Threat model

| # | Threat | Vector | Consequence |
|---|---|---|---|
| T1 | Agent-generated code is malicious or wrong | Model output | Host compromise, data loss |
| T2 | **Prompt injection via repo content** | README, comments, test fixtures | Agent acts against operator intent |
| T3 | Prompt injection via tool output | MCP response, search result | Same, with attacker-chosen payload |
| T4 | MCP server supply chain | Third-party server | Full tool-surface compromise |
| T5 | Exfiltration | Any egress path | Source code / secret loss |
| T6 | Cross-tenant leakage | Shared cache, shared FS | Confidentiality breach |
| T7 | Resource exhaustion | Runaway loop, fork bomb | Denial of service |

**T2/T3 are the defining hazard of an agentic system.** A coding agent's entire job is to
read untrusted text and act on it. There is no known complete defense — which is exactly why
the architecture assumes injection *sometimes succeeds* and constrains blast radius instead.

## 2. Defense in depth

```
 L1  Provenance      every observation tagged trusted | untrusted at ingest
 L2  Policy          evaluated on the ACTION, never on the text that motivated it
 L3  Isolation       strongest available tier per session; assume code inside is hostile
 L4  Egress          default-deny network; broker is the only path out
 L5  Detection       log inspection + anomaly detection on action streams
 L6  Recovery        event-sourced replay; deterministic incident reconstruction
```

The key design decision is **L2**: policy never asks "does this look like a legitimate
request?" It asks "is this action permitted for this session, regardless of why the model
wants it?" That distinction is what makes injection survivable — a successfully injected
agent still cannot exceed its granted authority.

## 3. Isolation tiers

| Tier | Mechanism | Boundary | Overhead | Use |
|---|---|---|---|---|
| I0 | Process + seccomp/Landlock | Weak | ~0 | Never for untrusted code |
| I1 | Container (OCI) | Namespace | Low | Trusted internal only |
| I2 | **gVisor (runsc)** | Userspace kernel | ~10-20% | **What `vm` builds today** |
| I3 | Firecracker / Kata microVM | Hardware virt | ~50-150 ms boot | Target, not shipped |
| I4 | Dedicated node | Physical | High | Classified / cross-tenant-sensitive |

**What ships today.** The four configurable tiers are `none`, `process`, `container` and
`vm`. `process` is bubblewrap on Linux and the Seatbelt sandbox on macOS; `container` is an
OCI container through the host's engine; `vm` is that same container pinned to the gVisor
`runsc` runtime, which is I2 above — a userspace kernel intercepting syscalls, not a
hardware-virtualised microVM. `internal/sandbox` contains exactly two backends, `process.go`
and `container.go`; there is no Firecracker or Kata implementation, and the tier refuses to
start if `runsc` is not registered with the container engine rather than silently running
without it.

**Why the naming, and what it costs you.** `vm` names the strongest tier the harness can
select, so configuration does not have to change when a hardware-virtualised backend lands;
the name is a slot, not a promise about the mechanism. A reviewer comparing boundaries
should read it as gVisor. Container-only isolation (I1) remains *not* sufficient for
agent-generated code — a container shares the host kernel — which is why `vm` exists as a
distinct tier and why `min_tier` is the setting that matters.

**I3 is on the roadmap, and is not implemented.** A microVM per session would give a
hardware-enforced boundary at a boot cost small relative to agent turn latency. Until it
exists, this document says gVisor.

Each sandboxed session gets: scoped filesystem (workspace only, no host mounts), no network by
default, CPU/memory/PID/disk quotas, wall-clock lifetime cap, and destruction on session end.
**VMs are never reused across tenants** — reuse is how T6 happens.

## 4. Prompt-injection controls

Layered, because no single control is sufficient:

1. **Provenance tagging.** Repo content, tool output, MCP responses, and search results
   enter as `untrusted` and stay tagged through the event store. Never concatenate untrusted
   text into a system prompt.
2. **Structural separation.** Untrusted content is delivered in a distinct message role or
   delimited block, never spliced into instructions.
3. **Action-level policy (L2).** Deny rules are absolute and survive every permission mode.
4. **Sensitive-action confirmation.** Destructive filesystem ops, credential access, egress,
   and privilege changes require explicit approval regardless of mode — no mode auto-approves
   them.
5. **Egress default-deny.** Even a fully injected agent has nowhere to send data.
6. **Anomaly detection.** Alert on action-sequence patterns inconsistent with the stated task
   (mass file reads, unexpected network attempts, credential-path access).

**What Abhed explicitly does not claim:** that it detects prompt injection reliably.
Detection is a mitigation layer, not the boundary. The boundary is L3 and L4.

## 5. MCP supply chain (T4)

MCP research was also unverified, so treat every third-party server as hostile until reviewed:

- **Registry with review gate.** No server runs that isn't in the signed internal registry.
- **Pin by digest**, never by tag or `latest`.
- **Least privilege per server** — its own credentials, its own network policy, its own
  isolation tier. A wiki-reader server has no reason to reach a database.
- **Tool-definition review.** Tool descriptions enter the model's context and are therefore
  an injection surface in themselves ("tool poisoning"). Review descriptions like code.
- **Runtime containment.** MCP servers run in I2 minimum, with declared egress only.

## 6. Tool surface discipline

The harness research surfaced a relevant number: cutting a tool set from 15 tools to 2 moved
task success from 80% to 100% [medium confidence — vendor-reported]. Fewer, better-scoped
tools improve both reliability *and* security: every tool is an attack surface and a
decision the model can get wrong.

Abhed ships a deliberately small native tool set — read, write, edit, glob, grep, bash,
task/subagent, plan — and everything else arrives through the reviewed MCP gateway.

### Opt-in tools execute outside the sandbox

`bash` runs inside the configured sandbox tier. The opt-in network tools — `ssh`, the
`k8s_*` tools, `websearch` and remote RAG — do not: they run in the host process with host
network, so the Seatbelt, bubblewrap or gVisor boundary that contains `bash` does not
contain them.

All four are disabled unless configured, so the default posture of no egress is intact.
Enabling one is a deliberate decision to move that execution and its egress outside the
boundary, and the consequences are worth stating plainly:

| Tool | Mediation once enabled |
|---|---|
| `ssh` | Always asks — it reports `Mutates() = true`, so no mode auto-approves it |
| `k8s_get`, `websearch` | Read-only, so **auto mode approves them without a prompt** |
| `k8s_apply` | Mutating, so it asks |

The read-only pair is the sharp edge: in an unattended or `auto` deployment, an injected
instruction in untrusted content can drive them to read and to reach the network with no
sandbox boundary in the way. Argument-scoped policy rules (`deny k8s_get(secrets*)`) are
the control that applies, and they work — but whole-tool policy is otherwise the only
thing mediating these tools.

Routing network-bound tools through a broker egress path, so egress stays default-deny and
auditable even when a tool is enabled, is tracked as outstanding work rather than shipped.

## 7. Validation status

Since nothing here is research-backed, isolation is *demonstrated* by tests rather than
asserted. Current state:

- [x] **Filesystem escape** — writes outside the workspace blocked; `/etc`, `/usr`, `/bin`
      unwritable (`TestProcessSandboxBlocksWriteOutsideWorkspace`, `...SystemPathWrite`)
- [x] **Egress** — network denied by default, verified from inside the sandbox
      (`TestProcessSandboxBlocksNetworkByDefault`)
- [x] **Credential access** — `~/.ssh`, `~/.aws`, `~/.kube` unreadable
- [x] **Resource exhaustion** — runaway commands bounded (`TestResourceLimitsRejectForkBomb`)
- [x] **Tier honesty** — no silent downgrade; `Select` fails with what it tried
      (`TestSelectRefusesToDowngrade`)
- [x] **Cross-tenant leakage** — session list and replay isolated (`server`)
- [x] **MCP tool poisoning** — descriptions sanitized before reaching the model
      (`internal/mcp`)
- [x] **Automated adversarial suite** — 24 attacks in `internal/redteam`, each written
      from the attacker's side so it fails when the attack succeeds: path traversal (9
      forms), symlink escape, blind overwrite, policy bypass (11 destructive commands),
      deny escalation across all modes, managed-policy override, scope widening, sandbox
      escape (5 techniques), exfiltration (4 channels), credential theft, prompt injection,
      MCP tool poisoning, ReDoS, runaway loop
- [x] **Injection corpus** — adversarial tasks in `internal/eval/corpus`, where completing
      the task is the failure
- [x] **Policy bypass** — escalation attempts covered by the adversarial suite
- [x] **Chained and race attacks** — 8 further attacks in `internal/redteam/chained_test.go`:
      undo abuse, export traversal, read-edit TOCTOU, concurrent-write tearing,
      subagent budget escape, command-wrapping bypass, defence-in-depth after a policy
      miss, session-id guessing
- [ ] **Human red-team engagement** — *still outstanding, and not substitutable.*
      Scoped and ready to commission: [`docs/ops/red-team-scope.md`](../ops/red-team-scope.md)

**On the automated suite's limits.** It proves the controls resist the attacks enumerated
in it. It cannot prove a determined attacker fails, because it only tries what its author
thought of — which is precisely the gap a human engagement exists to close. Treat a green
suite as a regression gate, not as clearance.

The suite has already earned that framing twice. Writing the chained attacks found
`find . -delete` slipping past the destructive-command patterns: it deletes recursively
while containing no `rm`. Fixed, along with `xargs rm`, `shred`, `truncate -s 0` and
`git checkout -- .`. **But the lesson is the pattern list, not the patch** — shell affords
endless ways to express deletion, so text matching will always lag. That is exactly why
the sandbox rather than the policy engine is the boundary, and the suite now asserts
containment *after* an assumed policy miss.

**Remaining position:** the process tier is a real filesystem and network boundary but
shares the host kernel. For genuinely untrusted repositories, set `sandbox.min_tier` to
`container` or `vm` and commission a human engagement first. Abhed will refuse to start
rather than silently downgrade below the tier you configure.
