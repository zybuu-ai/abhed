# Security Policy

Abhed is built and maintained by a small team at Zybuu. This document says
what we support, how to report a problem, and what you can expect back.

## Supported versions

| Version | Supported |
|---|---|
| 1.x, latest release | Yes |
| 1.x, earlier releases | Upgrade to the latest 1.x first |
| 0.x | No |

There is no long-term-support branch. Every fix lands on `main` and ships in
the next 1.x release. If you are running an older release or commit, the first
thing we will ask is whether the issue reproduces on the latest release.

## Reporting a vulnerability

Email **security@zybuu.com**.

If you do not get a response and need a fallback, use **support@zybuu.com** —
that is the address currently monitored day to day, and it reaches the same
person.

Please include:

- What you found and why it matters (impact, not just mechanism).
- Steps to reproduce, or a proof of concept.
- The commit or version you tested against.
- Whether the finding is against the harness itself, or against the hosted
  console at `abhed.zybuu.com` or the `zybuu.com` site that Zybuu operates.

Do not open a public GitHub issue for a security report. Use email so the
report isn't public before a fix ships.

## What to expect

- **Acknowledgement within 3 business days.**
- **A fix or a mitigation plan within 30 days for high-severity reports.**
  Lower-severity reports may take longer; we will tell you the plan, not
  leave you guessing.
- Credit in the fix's changelog entry or commit message, if you want it.
- A small team is doing this work. Response times are honest estimates, not
  contractual commitments.

## Safe harbor

We consider good-faith security research conducted under this policy to be
authorized:

- We will not pursue legal action against you for research that stays within
  this scope, avoids privacy violations and service disruption, and is
  reported to us before any public disclosure.
- If a third party (for example, a cloud or hosting provider) initiates
  action against you for research conducted in accordance with this policy,
  we will make it known that your actions were authorized.
- This safe harbor does not extend to attacks on other users, attempts to
  access another account's data, or anything covered by "out of scope"
  below. Do this kind of research against your own install or your own
  account, not someone else's.

We ask that you:

- Give us a reasonable time to investigate and remediate before any public
  disclosure.
- Make a good-faith effort to avoid privacy violations, data destruction, and
  interruption of the service for other people.
- Only interact with accounts you own or have explicit permission to test.

## Scope

In scope:

- **The harness** — everything under `cmd/`, `internal/`, and `sdk/`: the
  agent loop, the policy engine (`internal/policy`), the sandbox
  (`internal/sandbox`), the MCP gateway, the auth layer (`auth`),
  the Postgres store (`store`), and the server (`server`).
- **The console** — the web UI served by `abhed serve`.
- **The hosted console and site** — the hosted console at `abhed.zybuu.com`
  and the `zybuu.com` site, which Zybuu operates. Their deployment scripts
  and site code live in a separate private repository; findings against them
  are welcome at the same address.

## Out of scope

- **The model's own behaviour.** Abhed is model-agnostic (`docs/vision.md`);
  what a given model chooses to say or do, hallucination, or bias in its
  output is not a Abhed vulnerability. If Abhed's policy engine or sandbox
  fails to *contain* a model's action, that is in scope — the containment
  failure, not the model's decision.
- **Third-party providers.** The behavior, availability, or security of
  model endpoints, identity providers, Cloudflare, Resend, or any other
  service an operator configures Abhed to talk to. Report those to the
  provider; report to us only where Abhed's own handling of their response
  is the problem (for example, treating their output as untrusted per
  `docs/architecture/03-security.md`).
- Findings that require an operator to have already misconfigured Abhed in a
  way the documentation explicitly warns against (for example, running a
  multi-user server with `sandbox.min_tier: none`, which
  `docs/trust/security-posture.md` says not to do).
