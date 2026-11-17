// A TypeScript client for Abhed, the secure harness for deep agents, over
// `abhed rpc`. Policy, the sandbox, the record and the answer checks are the
// engine's: this package adds no way around them.

export { Abhed, Session, type SpawnOptions, type StartOptions, type StreamItem } from "./client.js";
export {
  AbhedError,
  Decision,
  ProcessExited,
  RunError,
  type Answer,
  type ApprovalRequest,
  type ClaimVerdict,
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
export { argsHidden, hasHidden, visible } from "./visible.js";
