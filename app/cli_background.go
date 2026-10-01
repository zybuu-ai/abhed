package app

import (
	"context"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/tasks", Args: "[cancel <id|all>]", Help: "list background tasks, or cancel them", Group: "background", Order: 70, Run: legacy("/tasks", slashTasks)})
	registerSlash(slashCmd{Name: "/wake", Args: "[off|notify|auto]", Help: "show or set what a background result does while idle", Group: "background", Order: 80, Run: legacy("/wake", slashWake)})
}

// liveTasks is how many background tasks the conversation has running.
func (c *cliState) liveTasks() int {
	if c.loop == nil {
		return 0
	}
	return c.loop.Background.Live()
}

// backgroundIdle reports whether nothing is left to wait for: no background
// task running, no result waiting to be delivered.
func (c *cliState) backgroundIdle() bool {
	return c.loop == nil || c.loop.Background.Live() == 0 && c.loop.Background.Pending() == 0
}

// endBackground ends the conversation's background work as the conversation
// closes (exit, /clear, /resume): every task is cancelled as session_closed,
// and the count is said.
func (c *cliState) endBackground() {
	if c.loop != nil && c.loop.Background != nil {
		n := c.loop.Background.Live()
		if n > 0 {
			fmt.Printf("  cancelling %d background task(s)\n", n)
		}
		c.loop.Background.Close(agent.TermSessionClosed)
		ended := 0
		for _, t := range c.loop.Background.Tasks() {
			if t.Reason == string(agent.TermSessionClosed) {
				ended++
			}
		}
		if n > 0 {
			fmt.Printf("  %d background task(s) ended as the session closed\n", ended)
		}
	}
	if c.unfollow != nil {
		c.unfollow()
		c.unfollow = nil
	}
}

// tasksCommand is /tasks: the conversation's background tasks, or cancelling
// one or all of them, as a stop by the person.
func tasksCommand(args []string, st *cliState, s ui.Style) {
	if st.loop == nil {
		fmt.Println(s.Dim("  no background tasks"))
		return
	}
	b := st.loop.Background
	if len(args) >= 2 && args[0] == "cancel" {
		if args[1] == "all" {
			fmt.Printf("  %s\n", s.Dim(fmt.Sprintf("cancelled %d background task(s)", b.CancelAll(agent.TermUserInterrupt))))
			return
		}
		if !b.Cancel(args[1], agent.TermUserInterrupt) {
			fmt.Printf("  %s no running task %s\n", s.Red("✕"), args[1])
			return
		}
		fmt.Printf("  %s\n", s.Dim("cancelled "+args[1]))
		return
	}
	list := b.Tasks()
	if len(list) == 0 {
		fmt.Println(s.Dim("  no background tasks"))
		return
	}
	for _, t := range list {
		status := t.Status
		if t.ExitCode != nil {
			status = fmt.Sprintf("%s %d", status, *t.ExitCode)
		}
		fmt.Printf("  %s  %-5s %-10s %s\n", t.ID, t.Kind, status, t.Description)
		if t.LastLine != "" {
			fmt.Printf("  %s\n", s.Dim("  "+t.LastLine))
		}
	}
}

// slashTasks is /tasks.
func slashTasks(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	tasksCommand(fields[1:], st, s)
	return false
}

// slashWake is /wake.
func slashWake(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	if st.loop == nil {
		fmt.Println(s.Dim("  no conversation yet; background tasks start with one"))
		return false
	}
	if len(fields) < 2 {
		fmt.Printf("  %s\n", s.Dim("wake: "+string(st.loop.Background.Mode())))
		return false
	}
	if err := st.loop.Background.SetWake(agent.WakeMode(fields[1]), agent.ByUser); err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
		return false
	}
	fmt.Printf("  %s\n", s.Dim("wake: "+fields[1]))
	return false
}
