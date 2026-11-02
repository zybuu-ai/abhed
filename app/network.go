package app

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/zybuu-ai/abhed/config"
)

// loadNetworked loads the configuration for a command that reaches the
// network and puts its upstream proxy in force before anything connects.
// Every command that makes a connection of Abhed's own loads through it.
func loadNetworked(workspace string, o config.LoadOptions) (config.Config, error) {
	cfg, err := config.LoadWith(workspace, o)
	if err != nil {
		return cfg, err
	}
	return cfg, cfg.ApplyNetwork()
}

// profileNetwork puts the person's and the managed configuration's upstream
// proxy in force for a command with no workspace that calls a server.
func profileNetwork(errOut io.Writer) bool {
	home, err := os.UserHomeDir()
	if err == nil {
		_, err = loadNetworked(home, config.LoadOptions{Quiet: true})
	}
	if err != nil {
		fmt.Fprintf(errOut, "abhed: the network configuration: %v\n", err)
		return false
	}
	return true
}

// orgPolicyAfterNetwork puts the upstream proxy in force, then refreshes the
// org policy as a session starts, so the fetch goes the way the session's
// calls will. An org policy never sets the network, so the session's own
// load later applies the same proxy. When the proxy cannot go in force,
// nothing is fetched and the error is returned.
func orgPolicyAfterNetwork(ctx context.Context, workspace string, o config.LoadOptions, errOut io.Writer) error {
	if _, err := loadNetworked(workspace, o); err != nil {
		return err
	}
	fetchOrgAtStart(ctx, errOut)
	return nil
}

// fetchOrgAtStart and startOrgRefresh are orgPolicyAtStart and
// keepOrgPolicyFresh; tests replace them to see whether they run.
var (
	fetchOrgAtStart = orgPolicyAtStart
	startOrgRefresh = keepOrgPolicyFresh
)

// orgPolicyAtConnect is an editor connection's start: the person's proxy,
// then the org policy fetch and its periodic refresh. With a network
// configuration that does not go in force neither runs, and noNetwork keeps
// each session's start from fetching around the proxy too.
func (c *acpConn) orgPolicyAtConnect(errOut io.Writer) {
	home, err := os.UserHomeDir()
	if err == nil {
		err = orgPolicyAfterNetwork(c.ctx, home, config.LoadOptions{Quiet: true}, errOut)
	}
	if err != nil {
		c.noNetwork = true
		fmt.Fprintf(errOut, "abhed: the network configuration: %v; the org policy is not fetched\n", err)
		return
	}
	if mc, err := config.MachineOrgPolicy(); err == nil {
		startOrgRefresh(c.ctx, mc.Refresh(), errOut)
	}
}
