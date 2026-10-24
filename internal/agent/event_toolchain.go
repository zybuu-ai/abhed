package agent

// Events of a language toolchain installed for a session's commands.
const (
	// EvToolchainInstall is an attempt to install a toolchain; see ToolchainAction.
	EvToolchainInstall EventType = "toolchain.install"
)

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
	Lang     string `json:"lang"`
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
