// The client against the real `abhed rpc`, on a scripted model. Skipped
// unless ABHED_BIN names an abhed binary. The model is a local
// OpenAI-compatible stub on 127.0.0.1, HOME is a temporary folder, and the
// workspace's configuration is the test's own, so nothing outside the test is
// read or written and no model is contacted.

import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { Abhed, Decision } from "../dist/index.js";

const BIN = process.env.ABHED_BIN ?? "";
const skip = BIN ? false : "set ABHED_BIN to an abhed binary to run against the real engine";

const sse = (chunks) => chunks.map((c) => `data: ${JSON.stringify(c)}\n\n`).join("") + "data: [DONE]\n\n";

async function model(command) {
  let calls = 0;
  const srv = createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      calls++;
      const body =
        calls === 1
          ? sse([
              { choices: [{ delta: { tool_calls: [{ index: 0, id: "c1", function: { name: "bash", arguments: JSON.stringify({ command, description: "probe" }) } }] } }] },
              { choices: [{ delta: {}, finish_reason: "tool_calls" }] },
            ])
          : sse([{ choices: [{ delta: { content: "done" }, finish_reason: "stop" }] }]);
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.end(body);
    });
  });
  await new Promise((r) => srv.listen(0, "127.0.0.1", r));
  return { url: `http://127.0.0.1:${srv.address().port}/v1`, close: () => srv.close() };
}

function setup(t, url) {
  const tmp = mkdtempSync(join(tmpdir(), "abhed-sdk-e2e-"));
  t.after(() => rmSync(tmp, { recursive: true, force: true }));
  const home = join(tmp, "home");
  const ws = join(realpathSync(tmp), "ws");
  mkdirSync(home);
  mkdirSync(join(ws, ".abhed"), { recursive: true });
  writeFileSync(join(ws, ".abhed", "config.json"), JSON.stringify({ model: { default: "stub", providers: { stub: { type: "openai-compatible", base_url: url, model: "m" } } } }));
  const ab = Abhed.spawn({ binary: BIN, cwd: ws, trustWorkspace: true, env: { HOME: home, PATH: process.env.PATH ?? "/usr/bin:/bin", TMPDIR: tmp } });
  return { ab, ws };
}

test("an approved call runs and is recorded", { skip }, async (t) => {
  const m = await model("touch made-by-e2e");
  t.after(m.close);
  const { ab, ws } = setup(t, m.url);
  const asked = [];
  const events = [];
  assert.ok((await ab.providers()).includes("openai-compatible"));
  const s = await ab.start({ onApproval: (r) => (asked.push(r), Decision.allowOnce("e2e@example.com")), onEvent: (e) => events.push(e) });
  assert.equal(s.workspaceTrust.trusted, true);
  const a = await s.prompt("make a file");
  assert.equal(a.text, "done");
  assert.ok(a.verdict);
  assert.ok((await s.usage()).Turns >= 2);
  assert.match((await s.exportHtml()).toLowerCase(), /<!doctype html/);
  assert.deepEqual(await s.tasks(), []);
  assert.equal(await ab.close(), 0);
  assert.equal(asked.length, 1);
  assert.equal(asked[0].args.command, "touch made-by-e2e");
  assert.ok(asked[0].request_id);
  assert.equal(events.find((e) => e.type === "action.approved").payload.approver, "e2e@example.com");
  assert.ok(existsSync(join(ws, "made-by-e2e")));
});

test("a denied call does not run", { skip }, async (t) => {
  const m = await model("touch refused-e2e");
  t.after(m.close);
  const { ab, ws } = setup(t, m.url);
  const s = await ab.start({ approvals: true });
  const kinds = [];
  let last;
  for await (const item of s.stream("make a file")) {
    last = item;
    if (item.kind === "event") kinds.push(item.event.type);
    if (item.kind === "approval") await s.approve(item.approval.request_id, Decision.deny());
  }
  await ab.close();
  assert.equal(last.answer.text, "done");
  assert.ok(kinds.includes("action.denied"));
  assert.equal(existsSync(join(ws, "refused-e2e")), false);
});

test("without approvals the call is refused", { skip }, async (t) => {
  const m = await model("touch never-e2e");
  t.after(m.close);
  const { ab, ws } = setup(t, m.url);
  await (await ab.start()).prompt("make a file");
  await ab.close();
  assert.equal(existsSync(join(ws, "never-e2e")), false);
});
