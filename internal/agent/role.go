package agent

import (
	"strings"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// RoleTools cuts a session's tools to a role, as starting it as a subagent
// would, for a session run as that role. A name the session lacks refuses.
func RoleTools(reg *tools.Registry, def *Definition) (*tools.Registry, error) {
	return childTools(reg, def)
}

// RoleToolsFor cuts a registry that holds only part of the session's tools,
// such as the one its subagents start from, to a role: the role's names
// that registry lacks are skipped, never added.
func RoleToolsFor(reg *tools.Registry, def *Definition) *tools.Registry {
	if def.Tools != nil {
		var names []string
		for _, n := range def.Tools {
			if !strings.EqualFold(n, "recall") {
				names = append(names, n)
			}
		}
		reg, _ = reg.SubsetStrict(names)
	}
	if len(def.DisallowedTools) > 0 {
		reg = reg.Without(def.DisallowedTools)
	}
	if def.MCPServers != nil {
		keep := map[string]bool{}
		for _, s := range def.MCPServers {
			keep[s] = true
		}
		var drop []string
		for _, t := range reg.All() {
			if s := mcpServerOf(t); s != "" && !keep[s] {
				drop = append(drop, t.Name())
			}
		}
		reg = reg.Without(drop)
	}
	if def.Skills != nil {
		if t, ok := reg.Get("skill"); ok {
			n, narrows := t.(SkillNarrower)
			if len(def.Skills) == 0 || !narrows {
				reg = reg.Without([]string{"skill"})
			} else {
				cut, _ := n.NarrowSkills(def.Skills)
				reg = reg.Clone()
				reg.Add(cut)
			}
		}
	}
	return reg
}

// RoleEffort is a role's reasoning effort, never above the session's when
// the session sets one.
func RoleEffort(session model.EffortLevel, role string) model.EffortLevel {
	return childEffort(session, role)
}

// RoleMode is the session's mode narrowed by a role's permission mode.
func RoleMode(session policy.Mode, role string) policy.Mode {
	if role == "" || rankOf(policy.Mode(role)) >= rankOf(session) {
		return session
	}
	return policy.Mode(role)
}
