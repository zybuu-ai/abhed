package toolset

import (
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// RemoteReads gives pol what decides which ssh commands run unasked: the
// operator's ssh.read_commands, and the read-only classifier's hosts and
// whether it reads files' contents. Every surface (the CLI, the server, the
// SDK) calls it, so none can drift from another.
func RemoteReads(pol *policy.Engine, cfg config.Config) error {
	if err := pol.AllowRemoteReads(cfg.SSH.ReadCommands...); err != nil {
		return err
	}
	pol.RemoteHosts = cfg.SSHReadOnlyHosts()
	pol.RemoteContents = pol.RemoteHosts != nil && cfg.SSHReadContents()
	pol.ReadCommandsContents = cfg.SSHReadContents()
	return nil
}
