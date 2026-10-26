package agent

// Events of a language toolchain installed for a session's commands.
const (
	// EvToolchainInstall is an attempt to install a toolchain; see ToolchainAction.
	EvToolchainInstall EventType = "toolchain.install"
	// EvToolchainRegistry is package registries opened to commands; see ToolchainRegistry.
	EvToolchainRegistry EventType = "toolchain.registry"
)

// ToolchainRegistry is the payload of toolchain.registry: a language's
// package registries opened to the session's commands through its egress
// proxy, with the person's agreement to the language's toolchain.
type ToolchainRegistry struct {
	Lang  string   `json:"lang"`
	Hosts []string `json:"hosts"`
	// Consent is as ToolchainAction's; allow for sandbox.toolchains_registries allow.
	Consent string `json:"consent,omitempty"`
	By      string `json:"by"`
	Surface string `json:"surface,omitempty"`
}

// Outcomes of a ToolchainAction.
const (
	ToolchainOK        = "ok"        // installed, or found installed
	ToolchainFailed    = "failed"    // tried and failed; Error says why
	ToolchainDeclined  = "declined"  // the person said no
	ToolchainRefused   = "refused"   // not allowed here: sandbox.toolchains off, or not a known language or version
	ToolchainCancelled = "cancelled" // stopped part way
)

// ToolchainAction is the payload of toolchain.install: which toolchain, from
// where, checked against which published checksum, who agreed and how it
// ended.
type ToolchainAction struct {
	Lang string `json:"lang"`
	// Extra is set for a tool installed beside the language, such as its
	// debugger (delve, debugpy); Version is then the extra's.
	Extra    string `json:"extra,omitempty"`
	Version  string `json:"version,omitempty"`
	Platform string `json:"platform,omitempty"`
	Outcome  string `json:"outcome"`
	// Why is how the version was chosen: requested, pinned by a file, or latest stable.
	Why string `json:"why,omitempty"`
	// Source names the release source; URL is the download, Digest its
	// published checksum, Bytes its size.
	Source string `json:"source,omitempty"`
	URL    string `json:"url,omitempty"`
	Digest string `json:"digest,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	// BuiltWith names the toolchain an extra was built with here.
	BuiltWith string `json:"built_with,omitempty"`
	// Cached is set when it was installed already and nothing was downloaded.
	Cached bool `json:"cached,omitempty"`
	// Consent is how it was agreed: asked (an approval), session (an
	// earlier "always allow" for this language and version), auto
	// (sandbox.toolchains auto), preapproved (sandbox.toolchains_preapproved),
	// or the person's own act at the CLI or in Studio.
	Consent string `json:"consent,omitempty"`
	Error   string `json:"error,omitempty"`
	// By is agent for the toolchain tool, user for the CLI and Studio.
	By string `json:"by"`
	// Surface is cli, tui, server, acp or tool.
	Surface string `json:"surface,omitempty"`
}
