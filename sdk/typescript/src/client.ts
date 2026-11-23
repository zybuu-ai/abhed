// A client for `abhed rpc`: one child process, line-delimited JSON on its
// pipes. Nothing here opens a socket; the model, tools, policy and record are
// all the engine's.

import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { createInterface } from "node:readline";

import {
  AbhedError,
  ProcessExited,
  RunError,
  type Answer,
  type ApprovalRequest,
  type Decision,
  type Event,
  type Mode,
  type Runbook,
  type SteerResult,
  type TaskInfo,
  type Usage,
  type Verdict,
  type VerifyMode,
  type WakeMode,
  type Woken,
  type WorkspaceTrust,
} from "./types.js";

export type StreamItem =
  | { kind: "event"; event: Event }
  | { kind: "approval"; approval: ApprovalRequest }
  | { kind: "answer"; answer: Answer };

export interface SpawnOptions {
  /** The abhed executable; `abhed` on PATH by default. */
  binary?: string;
  /** The folder abhed starts in, the default workspace. */
  cwd?: string;
  /** Its environment, this process's by default. The model's key reaches the engine from here, never through the protocol. */
  env?: NodeJS.ProcessEnv;
  /** Pass -trust-workspace, trusting the workspace's own .abhed/config.json. Leave off for a checkout you did not write. */
  trustWorkspace?: boolean;
  /** Further arguments before `rpc`. */
  args?: string[];
  /** Told of protocol notes: an error line no request owns, a handler that threw. */
  onError?: (message: string) => void;
}

export interface StartOptions {
  workspace?: string;
  mode?: Mode | string;
  allow?: string[];
  deny?: string[];
  verify?: VerifyMode;
  wake?: WakeMode;
  runbook?: Runbook;
  /** Every recorded event, in order. */
  onEvent?: (event: Event) => void;
  /** Each call needing approval; what it returns answers. A throw refuses. */
  onApproval?: (request: ApprovalRequest) => Decision | Promise<Decision>;
  /** Take approvals with no handler: they are yielded by stream() for approve(). */
  approvals?: boolean;
  onWoken?: (woken: Woken) => void;
}

type Line = Record<string, unknown>;

interface Waiter {
  resolve: (v: Line) => void;
  reject: (e: Error) => void;
}

const STDERR_KEEP = 64 * 1024;

/** One `abhed rpc` process. Call close() when done. */
export class Abhed {
  private readonly proc: ChildProcessWithoutNullStreams;
  private nextId = 1;
  private readonly waiting = new Map<string, Waiter>();
  private closedWith?: ProcessExited;
  private stderrText = "";
  private session?: Session;
  private readonly onError?: (message: string) => void;
  private readonly exited: Promise<number | null>;

  static spawn(opts: SpawnOptions = {}): Abhed {
    return new Abhed(opts);
  }

  private constructor(opts: SpawnOptions) {
    const argv = [...(opts.trustWorkspace ? ["-trust-workspace"] : []), ...(opts.args ?? []), "rpc"];
    this.onError = opts.onError;
    this.proc = spawn(opts.binary ?? "abhed", argv, {
      cwd: opts.cwd,
      env: opts.env ?? process.env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    this.exited = new Promise((resolve) => {
      let spawnError: Error | undefined;
      this.proc.on("error", (e) => {
        spawnError = e;
        this.fail(new ProcessExited(`abhed rpc did not start: ${e.message}`, null, null, this.stderrText));
        resolve(null);
      });
      this.proc.on("close", (code, signal) => {
        if (!spawnError) {
          this.fail(new ProcessExited(`abhed rpc exited with code ${code ?? signal}`, code, signal, this.stderrText));
        }
        resolve(code);
      });
    });
    this.proc.stdin.on("error", () => {
      // A write after exit is reported by the close handler.
    });
    this.proc.stderr.setEncoding("utf8");
    this.proc.stderr.on("data", (chunk: string) => {
      this.stderrText = (this.stderrText + chunk).slice(-STDERR_KEEP);
    });
    const lines = createInterface({ input: this.proc.stdout, crlfDelay: Infinity });
    lines.on("line", (line) => this.route(line));
  }

  /** The last 64 KiB abhed wrote to stderr: its warnings and notes. */
  get stderr(): string {
    return this.stderrText;
  }

  /**
   * Open a session; one opened earlier on this process closes. Without
   * onApproval or approvals, a call that needs approval is refused, as in a
   * headless run.
   */
  async start(opts: StartOptions = {}): Promise<Session> {
    const req: Line = { method: "start" };
    for (const k of ["workspace", "mode", "verify", "wake"] as const) {
      if (opts[k]) req[k] = opts[k];
    }
    if (opts.allow?.length) req.allow = [...opts.allow];
    if (opts.deny?.length) req.deny = [...opts.deny];
    if (opts.runbook) req.runbook = opts.runbook;
    if (opts.approvals ?? opts.onApproval !== undefined) req.approvals = true;
    const s = new Session(this, opts);
    // Set first: events of the new session may arrive before its ready.
    const old = this.session;
    this.session = s;
    if (old) old.superseded = true;
    const r = await this.send(req);
    s.workspaceTrust = (r.workspace_trust as WorkspaceTrust | undefined) ?? { workspace: "", trusted: false, reason: "" };
    return s;
  }

  /** The model provider types this build of abhed supports. */
  async providers(): Promise<string[]> {
    const r = await this.send({ method: "providers" });
    return Array.isArray(r.providers) ? r.providers.map(String) : [];
  }

  /**
   * Quit the session and wait for the process to exit. Running background
   * tasks end as session_closed. A process still running after `timeoutMs`
   * is sent SIGTERM, which ends the running prompt and records its end, then
   * SIGKILL.
   */
  async close(timeoutMs = 30_000): Promise<number | null> {
    if (!this.closedWith) {
      this.send({ method: "quit" }).catch(() => undefined);
      this.proc.stdin.end();
    }
    const timer = (ms: number) => new Promise<"timeout">((r) => setTimeout(() => r("timeout"), ms).unref());
    if ((await Promise.race([this.exited, timer(timeoutMs)])) === "timeout") {
      this.proc.kill("SIGTERM");
      if ((await Promise.race([this.exited, timer(10_000)])) === "timeout") this.proc.kill("SIGKILL");
    }
    return this.exited;
  }

  /** @internal */
  send(req: Line): Promise<Line> {
    if (this.closedWith) return Promise.reject(this.closedWith);
    const id = `ts-${this.nextId++}`;
    return new Promise<Line>((resolve, reject) => {
      this.waiting.set(id, { resolve, reject });
      this.proc.stdin.write(JSON.stringify({ id, ...req }) + "\n");
    }).then((r) => {
      if (r.type === "error") {
        throw new RunError(String(r.error ?? ""), typeof r.answer === "string" ? r.answer : "", r.verdict as Verdict | undefined);
      }
      return r;
    });
  }

  /** @internal */
  answer(requestId: string, d: Decision): Promise<void> {
    const req: Line = { method: "approve", request_id: requestId, decision: d.allow ? "allow" : "deny" };
    if (d.remember) req.remember = true;
    if (d.approver) req.approver = d.approver;
    return this.send(req).then(() => undefined);
  }

  /** @internal */
  note(message: string): void {
    try {
      this.onError?.(message);
    } catch {
      // A note handler's failure is not the protocol's.
    }
  }

  private fail(err: ProcessExited): void {
    if (this.closedWith) return;
    this.closedWith = err;
    for (const w of this.waiting.values()) w.reject(err);
    this.waiting.clear();
    this.session?.ended(err);
  }

  private route(line: string): void {
    const text = line.trim();
    if (!text) return;
    let msg: Line;
    try {
      const v: unknown = JSON.parse(text);
      if (typeof v !== "object" || v === null || Array.isArray(v)) return;
      msg = v as Line;
    } catch {
      this.note(`abhed rpc wrote a line that is not JSON: ${text.slice(0, 200)}`);
      return;
    }
    if (typeof msg.id === "string" && msg.id) {
      const w = this.waiting.get(msg.id);
      if (w) {
        this.waiting.delete(msg.id);
        w.resolve(msg);
      }
      return;
    }
    const s = this.session;
    switch (msg.type) {
      case "event":
        if (s && msg.event && typeof msg.event === "object") s.event(msg.event as Event);
        break;
      case "approval":
        if (msg.approval && typeof msg.approval === "object") {
          const a = msg.approval as ApprovalRequest;
          if (s) s.approval(a);
          else this.answer(a.request_id, { allow: false }).catch(() => undefined);
        }
        break;
      case "woken":
        s?.woken({ answer: String(msg.answer ?? ""), ...(msg.error ? { error: String(msg.error) } : {}) });
        break;
      case "error":
        this.note(String(msg.error ?? ""));
        break;
    }
  }
}

type StreamSink = (item: StreamItem | Error | null) => void;

/** A session opened by Abhed.start. Every prompt continues it. */
export class Session {
  workspaceTrust: WorkspaceTrust = { workspace: "", trusted: false, reason: "" };
  /** @internal */
  superseded = false;
  private readonly recorded: Event[] = [];
  private readonly sinks = new Set<StreamSink>();

  /** @internal */
  constructor(
    private readonly client: Abhed,
    private readonly opts: StartOptions,
  ) {}

  /**
   * Send a prompt and wait for the final answer. A run that ended otherwise
   * than completed rejects with RunError; under verify "strict" an
   * unverified answer has `reason === "unverified"`.
   */
  async prompt(text: string): Promise<Answer> {
    return toAnswer(await this.send({ method: "prompt", prompt: text }));
  }

  /**
   * Send a prompt and yield its events as they are recorded, each approval
   * when the session takes approvals with no handler (answer it with
   * approve()), and last the answer. A failed run throws RunError after its
   * events.
   */
  async *stream(text: string): AsyncGenerator<StreamItem, void, undefined> {
    const queue: Array<StreamItem | Error | null> = [];
    let wake: (() => void) | undefined;
    const sink: StreamSink = (item) => {
      queue.push(item);
      wake?.();
    };
    this.sinks.add(sink);
    this.send({ method: "prompt", prompt: text }).then(
      (r) => {
        sink({ kind: "answer", answer: toAnswer(r) });
        sink(null);
      },
      (e: Error) => sink(e),
    );
    try {
      for (;;) {
        while (queue.length === 0) await new Promise<void>((r) => (wake = r));
        const item = queue.shift();
        if (item === null || item === undefined) return;
        if (item instanceof Error) throw item;
        yield item;
      }
    } finally {
      this.sinks.delete(sink);
    }
  }

  /** Redirect the running prompt: `steered` when it reads the text before answering, `queued` when the next prompt will. */
  async steer(text: string): Promise<SteerResult> {
    const r = await this.send({ method: "steer", prompt: text });
    return r.type === "steered" ? "steered" : "queued";
  }

  /** Answer an approval yielded by stream(). */
  approve(requestId: string, decision: Decision): Promise<void> {
    return this.client.answer(requestId, decision);
  }

  async usage(): Promise<Usage> {
    return (await this.send({ method: "usage" })).usage as Usage;
  }

  /** The session as a self-contained HTML transcript. */
  async exportHtml(): Promise<string> {
    return String((await this.send({ method: "export" })).answer ?? "");
  }

  async tasks(): Promise<TaskInfo[]> {
    const t = (await this.send({ method: "tasks" })).tasks;
    return Array.isArray(t) ? (t as TaskInfo[]) : [];
  }

  async cancelTask(taskId: string): Promise<void> {
    await this.send({ method: "cancel_task", task_id: taskId });
  }

  /** Run the agent on background results waiting for it. */
  async wake(): Promise<Answer> {
    return toAnswer(await this.send({ method: "wake" }));
  }

  /** Every event this session has delivered so far. */
  events(): Event[] {
    return [...this.recorded];
  }

  get sessionId(): string {
    return this.recorded.find((e) => e.session_id)?.session_id ?? "";
  }

  private send(req: Line): Promise<Line> {
    if (this.superseded) return Promise.reject(new AbhedError("this session was replaced by a later start"));
    return this.client.send(req);
  }

  /** @internal */
  event(ev: Event): void {
    this.recorded.push(ev);
    for (const s of this.sinks) s({ kind: "event", event: ev });
    if (this.opts.onEvent) {
      try {
        this.opts.onEvent(ev);
      } catch (e) {
        this.client.note(`onEvent threw: ${String(e)}`);
      }
    }
  }

  /** @internal */
  approval(req: ApprovalRequest): void {
    const handler = this.opts.onApproval;
    if (handler) {
      Promise.resolve()
        .then(() => handler(req))
        .catch((e: unknown) => {
          this.client.note(`onApproval threw, so the call is refused: ${String(e)}`);
          return { allow: false } as Decision;
        })
        .then((d) => this.client.answer(req.request_id, d && typeof d.allow === "boolean" ? d : { allow: false }))
        .catch((e: unknown) => this.client.note(`the approval could not be answered: ${String(e)}`));
      return;
    }
    if (this.sinks.size === 0) {
      // Nobody can answer it: refused rather than left waiting.
      this.client.note(`approval ${req.request_id} for ${req.tool} had no handler and no stream; refused`);
      this.client.answer(req.request_id, { allow: false }).catch(() => undefined);
      return;
    }
    for (const s of this.sinks) s({ kind: "approval", approval: req });
  }

  /** @internal */
  woken(w: Woken): void {
    try {
      this.opts.onWoken?.(w);
    } catch (e) {
      this.client.note(`onWoken threw: ${String(e)}`);
    }
  }

  /** @internal */
  ended(err: ProcessExited): void {
    for (const s of this.sinks) s(err);
  }
}

function toAnswer(r: Line): Answer {
  const a: Answer = { text: typeof r.answer === "string" ? r.answer : "" };
  if (r.verdict && typeof r.verdict === "object") a.verdict = r.verdict as Verdict;
  return a;
}
