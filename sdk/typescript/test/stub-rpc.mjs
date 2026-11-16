#!/usr/bin/env node
// A stand-in for `abhed rpc`: the protocol's shapes, with scripted answers.
// What each prompt does is chosen by its text, so the tests drive every kind
// of line the real engine sends without a model. The shapes follow
// app/rpc.go; e2e.test.mjs checks the same client against the real binary.

import { createInterface } from "node:readline";

const SID = "s-stub";
let seq = 0;
let startReq = null;
let approvals = false;
let started = false;

const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");
const event = (type, payload, actor = "agent") => {
  seq++;
  out({ type: "event", event: { id: `e${seq}`, session_id: SID, seq, type, payload, actor, trust: "trusted", created_at: "2026-11-17T00:00:00Z" } });
};
const VERIFIED = { mode: "label", source: "text", claims: [], verified: 0, unverified: 0, contradicted: 0, outcome: "verified", note: "nothing to check", turn: 1 };

// Lines are read one at a time; a prompt waiting for an approve or a steer
// takes the next line of that method and holds the others.
const lines = [];
const readers = [];
let eof = false;
const rl = createInterface({ input: process.stdin, crlfDelay: Infinity });
rl.on("line", (l) => {
  if (!l.trim()) return;
  const r = JSON.parse(l);
  const i = readers.findIndex((x) => x.want(r));
  if (i >= 0) readers.splice(i, 1)[0].resolve(r);
  else {
    lines.push(r);
    wakeMain?.();
  }
});
rl.on("close", () => {
  eof = true;
  for (const x of readers.splice(0)) x.resolve(null);
  wakeMain?.();
});
let wakeMain;
const next = async () => {
  while (lines.length === 0) {
    if (eof) return null;
    await new Promise((r) => (wakeMain = r));
  }
  return lines.shift();
};
const waitFor = (want) =>
  new Promise((resolve) => {
    if (eof) return resolve(null);
    readers.push({ want, resolve });
  });

async function prompt(r) {
  const text = r.prompt ?? "";
  const id = r.id;
  event("user.message", { text }, "user");
  switch (text) {
    case "hello":
      event("agent.message", { text: "hi" });
      event("answer.verdict", VERIFIED, "system");
      return out({ id, type: "answer", answer: "hi", verdict: VERIFIED });
    case "echo-start":
      return out({ id, type: "answer", answer: JSON.stringify(startReq) });
    case "approve-me": {
      const args = { command: "rm -rf build‮", description: "clean" };
      event("action.requested", { call_id: "c1", tool: "bash", args, requires_approval: true });
      if (!approvals) {
        event("action.denied", { call_id: "c1", by: "headless" }, "system");
        return out({ id, type: "answer", answer: "refused" });
      }
      out({ type: "approval", approval: { request_id: "r-1", call_id: "c1", tool: "bash", args, reason: "a destructive command", step: "destructive", rule: "builtin:destructive", destructive: true, args_hidden: true } });
      for (;;) {
        const a = await waitFor((x) => x.method === "approve");
        if (a === null) return event("action.denied", { call_id: "c1", by: "system", reason: "rpc input ended" }, "system");
        if (a.request_id !== "r-1") {
          out({ id: a.id, type: "error", error: `no approval is waiting with request_id ${a.request_id}` });
          continue;
        }
        out({ id: a.id, type: "answered" });
        if (a.decision === "allow") {
          event("action.approved", { call_id: "c1", by: "reviewer", approver: a.approver ?? "" }, "user");
          return out({ id, type: "answer", answer: `allowed by ${a.approver ?? ""}` });
        }
        event("action.denied", { call_id: "c1", by: "reviewer" }, "user");
        return out({ id, type: "answer", answer: `denied remember=${Boolean(a.remember)}` });
      }
    }
    case "strict": {
      const v = { ...VERIFIED, mode: "strict", outcome: "unverified", unverified: 1, claims: [{ n: 1, text: "tests pass", kind: "test", verdict: "unverified", reason: "no test run in the record", load_bearing: true }] };
      event("answer.verdict", v, "system");
      return out({ id, type: "error", error: "abhed: ended as unverified: 1 statement about the work is unverified", answer: "the tests pass", verdict: v });
    }
    case "steer-me": {
      event("agent.delta", { text: "working" });
      const s = await waitFor((x) => x.method === "steer");
      out({ id: s.id, type: "steered" });
      return out({ id, type: "answer", answer: `steered: ${s.prompt}` });
    }
    case "wake-later":
      out({ id, type: "answer", answer: "started" });
      event("subagent.notice", { task_id: "t1" }, "system");
      return out({ type: "woken", answer: "background done" });
    case "stray-error":
      out({ type: "error", error: "1 queued steer message(s) were not delivered: test" });
      return out({ id, type: "answer", answer: "ok" });
    case "die":
      process.stderr.write("abhed: boom\n");
      process.exit(3);
    default:
      return out({ id, type: "answer", answer: `echo: ${text}` });
  }
}

if (process.argv.at(-1) !== "rpc") {
  process.stderr.write("stub: want rpc\n");
  process.exit(2);
}

for (;;) {
  const r = await next();
  if (r === null) process.exit(0);
  const { method: m, id } = r;
  if (m === "start") {
    if (r.workspace === "/nope") {
      out({ id, type: "error", error: "abhed: workspace /nope does not exist" });
      continue;
    }
    startReq = r;
    approvals = Boolean(r.approvals);
    started = true;
    out({ id, type: "ready", workspace_trust: { workspace: r.workspace ?? "/w", trusted: false, reason: "none" } });
  } else if (m === "providers") out({ id, type: "providers", providers: ["anthropic", "openai-compatible"] });
  else if (m === "quit") {
    out({ id, type: "bye" });
    process.exit(0);
  } else if (m === "approve") out({ id, type: "error", error: `no approval is waiting with request_id ${r.request_id}` });
  else if (m === "steer") out({ id, type: "queued" });
  else if (!started) out({ id, type: "error", error: "no session: send start first" });
  else if (m === "prompt") await prompt(r);
  else if (m === "usage") out({ id, type: "usage", usage: { InputTokens: 120, OutputTokens: 8, CachedTokens: 64, Turns: 2, Compactions: 0, CostUSD: 0.0012, Unpriced: false } });
  else if (m === "export") out({ id, type: "export", answer: "<!doctype html><title>s-stub</title>" });
  else if (m === "tasks") out({ id, type: "tasks", tasks: [{ task_id: "t1", kind: "task", description: "scan", status: "running", started: "2026-11-17T00:00:00Z" }] });
  else if (m === "cancel_task") {
    if (r.task_id === "t1") out({ id, type: "cancelled" });
    else out({ id, type: "error", error: `no background task ${r.task_id}` });
  } else if (m === "wake") out({ id, type: "error", error: "nothing to wake" });
  else out({ id, type: "error", error: `unknown method ${JSON.stringify(m)}` });
}
