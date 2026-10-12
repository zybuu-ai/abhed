package plugin

import (
	"context"
	"os/exec"

	"github.com/zybuu-ai/abhed/driver"
)

// commands is false here: the bridge needs a Unix terminal.
func (cl *Client) commands() bool { return false }

// Command is not available on Windows.
func (cl *Client) Command(context.Context, string, driver.CommandRequest) (*exec.Cmd, error) {
	return nil, cl.fail(driver.ErrUnsupported, "a session's commands are not supported on Windows")
}

// ForwardEnv does nothing on Windows.
func (cl *Client) ForwardEnv(*exec.Cmd, []string) {}
