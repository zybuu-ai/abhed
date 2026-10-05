package agent

// Events the interactive CLI records about the person's own actions: modes,
// rules, directories, mentions, commands, memory, session names and branches,
// restores, plans, model fallback, hooks and record repair. Rewind reuses
// conversation.forked and adds file.restored.
const (
	// EvModeChanged is a permission mode change; see ModeChanged.
	EvModeChanged EventType = "mode.changed"
	// EvPermissionChanged is a session rule added or removed; see PermissionChanged.
	EvPermissionChanged EventType = "permission.changed"
	// EvWorkspaceDirAdded is a directory added to the session; see WorkspaceDirAdded.
	EvWorkspaceDirAdded EventType = "workspace.dir_added"
	// EvInputMention is a file attached to a message by @; see InputMention.
	EvInputMention EventType = "input.mention"
	// EvCommandInvoked is a slash command run; see CommandInvoked.
	EvCommandInvoked EventType = "command.invoked"
	// EvMemoryLoaded lists the memory files put into the prompt; see MemoryLoaded.
	EvMemoryLoaded EventType = "memory.loaded"
	// EvMemoryWritten is a memory file written; see MemoryWritten.
	EvMemoryWritten EventType = "memory.written"
	// EvSessionNamed is a session given a name; see SessionNamed.
	EvSessionNamed EventType = "session.named"
	// EvSessionBranched is a session copied into a new one; see SessionBranched.
	EvSessionBranched EventType = "session.branched"
	// EvFileRestored is a file put back from a checkpoint; see FileRestored.
	EvFileRestored EventType = "file.restored"
	// EvPlanProposed is a plan the agent submitted in plan mode; see PlanProposed.
	EvPlanProposed EventType = "plan.proposed"
	// EvPlanDecided is the person's answer to a proposed plan; see PlanDecided.
	EvPlanDecided EventType = "plan.decided"
	// EvModelFallback is a move to a fallback model; see ModelFallback.
	EvModelFallback EventType = "model.fallback"
	// EvHookFired is an extension hook's verdict; see HookFired.
	EvHookFired EventType = "hook.fired"
	// EvRecordRepaired is a record whose torn end was cut off on open; see RecordRepaired.
	EvRecordRepaired EventType = "record.repaired"
)

// Events the editor protocol records about the person's own actions in Abhed
// Studio (docs/architecture/studio-acp-contract.md).
const (
	// EvApprovalScopeGranted is an "always allow" a person chose; see ScopeGranted.
	EvApprovalScopeGranted EventType = "approval.scope_granted"
	// EvManualEdit is a person's own save of a file; see ManualEdit.
	EvManualEdit EventType = "manual.edit"
	// EvTaskCancelled is a person stopping one background task; see TaskCancelled.
	EvTaskCancelled EventType = "task.cancelled"
	// EvMCPStatus is an MCP server's connection changed by a person; see MCPStatus.
	EvMCPStatus EventType = "mcp.status"
)

// MCPStatus is the payload of mcp.status: what a person did to one MCP
// server's connection, and how it ended.
type MCPStatus struct {
	Server string `json:"server"`
	Op     string `json:"op"`     // restart
	Status string `json:"status"` // connected or error
	Error  string `json:"error,omitempty"`
	By     string `json:"by"`
}

// ScopeGranted is the payload of approval.scope_granted.
type ScopeGranted struct {
	Scope string `json:"scope"`
	By    string `json:"by"`
}

// ManualEdit is the payload of manual.edit: a save the person made in their
// editor, recorded, not judged, since it is their own file on their machine.
type ManualEdit struct {
	Path         string `json:"path"`
	BeforeSHA256 string `json:"before_sha256,omitempty"`
	AfterSHA256  string `json:"after_sha256,omitempty"`
	Patch        string `json:"patch,omitempty"`
	By           string `json:"by"`
}

// TaskCancelled is the payload of task.cancelled.
type TaskCancelled struct {
	TaskID string `json:"task_id"`
	By     string `json:"by"`
}

// How a mode change was made, as ModeChanged.Via records it.
const (
	ViaFlag     = "flag"
	ViaSlash    = "slash"
	ViaShiftTab = "shift-tab"
	ViaPlanExit = "plan-exit"
	ViaStudio   = "studio"
)

// ModeChanged is the payload of mode.changed.
type ModeChanged struct {
	From string `json:"from"`
	To   string `json:"to"`
	By   string `json:"by"`  // who: user, or the managed configuration
	Via  string `json:"via"` // flag, slash, shift-tab or plan-exit
}

// PermissionChanged is the payload of permission.changed: a rule added to or
// removed from the session's own rules, which end with the session.
type PermissionChanged struct {
	Op    string `json:"op"`    // add or remove
	List  string `json:"list"`  // allow, ask or deny
	Rule  string `json:"rule"`  // e.g. bash(go test*)
	Scope string `json:"scope"` // session
	By    string `json:"by"`
}

// WorkspaceDirAdded is the payload of workspace.dir_added.
type WorkspaceDirAdded struct {
	Path      string `json:"path"`      // as typed
	Canonical string `json:"canonical"` // symlinks resolved
	Access    string `json:"access"`    // read, or read-write
	By        string `json:"by"`
}

// InputMention is the payload of input.mention: what was attached, not the
// content, which the message itself carries.
type InputMention struct {
	Path string `json:"path"`
	// Range is the lines attached, as "10-20"; "" for the whole file.
	Range     string `json:"range,omitempty"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
	Truncated bool   `json:"truncated,omitempty"`
}

// CommandInvoked is the payload of command.invoked. Args are redacted before
// they are recorded.
type CommandInvoked struct {
	Name   string `json:"name"`
	Source string `json:"source"` // builtin, user, workspace, managed or mcp
	// SHA256 is the hash of a command file's content, for those that have one.
	SHA256 string `json:"sha256,omitempty"`
	Args   string `json:"args,omitempty"`
}

// MemoryFile is one memory file put into the prompt.
type MemoryFile struct {
	Path   string `json:"path"`
	Scope  string `json:"scope"` // managed, user, project, local, subdirectory or rule
	SHA256 string `json:"sha256"`
}

// MemoryLoaded is the payload of memory.loaded.
type MemoryLoaded struct {
	Files []MemoryFile `json:"files"`
}

// MemoryWritten is the payload of memory.written.
type MemoryWritten struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // user, feedback, project, reference, or note
	By   string `json:"by"`   // user, or agent
}

// SessionNamed is the payload of session.named.
type SessionNamed struct {
	Name string `json:"name"`
}

// SessionBranched is the payload of session.branched, recorded in the new
// session: it names the session it was copied from and the last seq taken.
type SessionBranched struct {
	From       string `json:"from"`
	ThroughSeq int64  `json:"through_seq"`
	// Unverified says why the source's record failed verification, when a
	// person chose to go on from it; "" when it verified.
	Unverified string `json:"unverified,omitempty"`
}

// FileRestored is the payload of file.restored.
type FileRestored struct {
	Path         string `json:"path"`
	BeforeSHA256 string `json:"before_sha256,omitempty"` // "" when the file did not exist
	AfterSHA256  string `json:"after_sha256,omitempty"`  // "" when the restore removed it
	Checkpoint   string `json:"checkpoint"`
	By           string `json:"by"` // user
}

// PlanProposed is the payload of plan.proposed.
type PlanProposed struct {
	Text string `json:"text"`
}

// PlanDecided is the payload of plan.decided.
type PlanDecided struct {
	Decision string `json:"decision"` // accept, or keep-planning
	ToMode   string `json:"to_mode,omitempty"`
}

// ModelFallback is the payload of model.fallback.
type ModelFallback struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// HookFired is the payload of hook.fired.
type HookFired struct {
	Extension string `json:"extension"`
	Event     string `json:"event"`
	Verdict   string `json:"verdict"` // block, ask or annotate; never allow
}

// RecordRepaired is the payload of record.repaired.
type RecordRepaired struct {
	Reason         string `json:"reason"`
	TruncatedBytes int64  `json:"truncated_bytes"`
}
