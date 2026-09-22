# Abhed documentation

Abhed is an on-prem deep agent harness. It runs where your data is, against whichever
model you point it at, and records everything it does.

## Start here

| Guide | For |
|---|---|
| [Getting started](01-getting-started.md) | install, first run, the shape of a session |
| [Configuration](02-configuration.md) | models, permissions, storage, every setting |
| [Models and providers](03-providers.md) | the twenty providers, sampling parameters, subscriptions |
| [Permissions and safety](04-permissions.md) | what the agent may do, and how you decide |

## Extending it

| Guide | For |
|---|---|
| [Tools](05-tools.md) | the built-in tools, and the four ways to add your own |
| [Skills](06-skills.md) | teaching Abhed a procedure it should follow |
| [Extensions](07-extensions.md) | intercepting, filtering and adding capability |
| [MCP](08-mcp.md) | connecting Model Context Protocol servers |

## Building on it

| Guide | For |
|---|---|
| [The SDK](09-sdk.md) | embedding Abhed in a Go program |
| [Automation](10-automation.md) | headless runs, `resolve` for issues on GitHub, GitLab and Gitea, RPC, and editors over ACP |
| [Sessions and audit](11-sessions.md) | replay, forking, export, what is recorded |
| [HawkEYE](15-hawkeye.md) | what a session did: tokens, the policy step behind each call, findings |
| [The workbench](16-workbench.md) | `/ide`: the agent beside the code, tools, extensions, terminal and findings |
| [Structured output](13-structured-output.md) | a typed answer that matches a schema, on every provider |
| [Parallel subagents](14-parallel-subagents.md) | several at once, each in its own git worktree |

Exporting the event log as OpenTelemetry traces and scheduled runs are
Enterprise Edition features, documented with that edition.
