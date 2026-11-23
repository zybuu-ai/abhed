// The values abhed rpc sends, typed. Each keeps the object it came from in
// `raw`, so a field the protocol adds later is readable before it is named here.

export type Mode = "default" | "plan" | "accept-edits" | "auto" | "bypass";
export type VerifyMode = "off" | "label" | "strict";
export type WakeMode = "off" | "notify" | "auto";
export type SteerResult = "steered" | "queued";

/** One recorded event, as the session's record holds it. */
export interface Event {
  id: string;
  session_id: string;
  parent_id?: string;
  seq: number;
  type: string;
  payload: Record<string, unknown>;
  actor: string;
  trust: string;
  created_at: string;
}

/** What the checks found for one statement of an answer. */
export interface ClaimVerdict {
  n: number;
  text: string;
  kind: string;
  verdict: string;
  reason: string;
  load_bearing?: boolean;
  evidence?: number[];
  cited?: string[];
  [key: string]: unknown;
}

/**
 * The check of an answer against what the session observed. `outcome` is
 * `verified` or `unverified`; `verified` means nothing about the work was left
 * unproven, not that every statement was checked, so read `note` too.
 */
export interface Verdict {
  mode: string;
  source: string;
  claims: ClaimVerdict[];
  verified: number;
  unverified: number;
  contradicted: number;
  outcome: "verified" | "unverified" | string;
  note?: string;
  turn?: number;
  [key: string]: unknown;
}

/** A prompt's final answer, with its verdict when answers are checked. */
export interface Answer {
  text: string;
  verdict?: Verdict;
}

/** Tokens, turns and cost so far in the session, as the engine names them. */
export interface Usage {
  InputTokens: number;
  OutputTokens: number;
  CachedTokens: number;
  Turns: number;
  Compactions: number;
  ColdPrefillTokens?: number;
  CostUSD: number;
  Unpriced: boolean;
  UnpricedCalls?: number;
}

/** One background task: a subagent (kind `task`) or a shell (`shell`). */
export interface TaskInfo {
  task_id: string;
  kind: string;
  description: string;
  status: string;
  reason?: string;
  summary?: string;
  started?: string;
  [key: string]: unknown;
}

/** Whether the workspace's `.abhed/config.json` applied whole. */
export interface WorkspaceTrust {
  workspace: string;
  trusted: boolean;
  reason: string;
  sha256?: string;
  [key: string]: unknown;
}

/**
 * A call that needs approval, put to the caller. `args`, `reason` and `offer`
 * are the copy the record holds, and the model's, unescaped: pass anything a
 * person reads through `visible()`, and consider refusing outright when
 * `args_hidden` is set.
 */
export interface ApprovalRequest {
  request_id: string;
  call_id?: string;
  tool: string;
  args: unknown;
  reason?: string;
  step?: string;
  rule?: string;
  source?: string;
  /** The scope a person may always allow; `remember` keeps it for the session. */
  offer?: string;
  destructive?: boolean;
  args_hidden?: boolean;
  subagent?: string;
  task_id?: string;
  via?: string;
}

/**
 * An answer to an ApprovalRequest. `remember` keeps the offered scope for the
 * rest of the session and is ignored when none was offered. `approver` is
 * recorded as you assert it: authenticate the person before you name them.
 */
export interface Decision {
  allow: boolean;
  remember?: boolean;
  approver?: string;
}

export const Decision = {
  allowOnce: (approver?: string): Decision => ({ allow: true, approver }),
  allowAlways: (approver?: string): Decision => ({ allow: true, remember: true, approver }),
  deny: (approver?: string): Decision => ({ allow: false, approver }),
};

/** The answer of a run a background result started between prompts. */
export interface Woken {
  answer: string;
  error?: string;
}

/** A plan's runbook, approved for the session (see the Runbooks guide). */
export interface Runbook {
  plan: string;
  runbook: Array<Record<string, unknown>>;
  sha256?: string;
}

/** Anything abhed rpc answered with an error, or a broken connection. */
export class AbhedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AbhedError";
  }
}

/** abhed rpc ended, or could not start, before answering. */
export class ProcessExited extends AbhedError {
  readonly code: number | null;
  readonly signal: string | null;
  readonly stderr: string;
  constructor(message: string, code: number | null, signal: string | null, stderr: string) {
    super(message);
    this.name = "ProcessExited";
    this.code = code;
    this.signal = signal;
    this.stderr = stderr;
  }
}

const ENDED = /^abhed: ended as ([a-z_]+)(?:: ([\s\S]*))?$/;

/**
 * A request abhed rpc answered with an error. For a prompt that ended
 * otherwise than completed, `reason` is the terminal reason (`unverified`,
 * `max_turns`, `prompt_refused`, ...) and `detail` what followed it; `answer`
 * and `verdict` are what the run produced, which may be set though it failed.
 */
export class RunError extends AbhedError {
  readonly reason: string;
  readonly detail: string;
  readonly answer: string;
  readonly verdict?: Verdict;
  constructor(message: string, answer = "", verdict?: Verdict) {
    super(message);
    this.name = "RunError";
    this.answer = answer;
    this.verdict = verdict;
    const m = ENDED.exec(message.split("\n", 1)[0] ?? "");
    this.reason = m?.[1] ?? "";
    this.detail = m?.[2] ?? "";
  }
  get unverified(): boolean {
    return this.reason === "unverified";
  }
}
