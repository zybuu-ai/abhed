package policy

import (
	"sort"
	"strings"
)

// builtinTools are the names Abhed gives its own tools and the actions a rule
// can name (an Explorer delete, mkdir or rename, a secret, a pull request).
// A rule may write one in any case; it is read in this spelling.
var builtinTools = map[string]bool{
	"bash": true, "read": true, "write": true, "edit": true, "glob": true, "grep": true,
	"todo": true, "ask_user": true, "memory_write": true, "exit_plan": true,
	"tool_search": true, "task": true, "tasks": true, "task_status": true,
	"task_cancel": true, "recall": true, "shell_output": true, "shell_kill": true,
	"web_search": true, "web_fetch": true, "search": true, "skill": true, "result": true,
	"k8s_get": true, "k8s_apply": true, "k8s_login": true, "ssh": true, "ssh_connect": true,
	"secret": true, "delete": true, "mkdir": true, "rename": true, "forge_pr": true,
}

// BuiltinTool reports whether name is one of Abhed's own tool or action names, as spelled.
func BuiltinTool(name string) bool { return builtinTools[name] }

// BuiltinTools are Abhed's own tool and action names, sorted.
func BuiltinTools() []string {
	out := make([]string, 0, len(builtinTools))
	for n := range builtinTools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ReservedToolName reports whether a tool from outside Abhed, such as an
// extension's, may not take name: in any case it is a built-in name. That is
// what lets a rule's tool name be folded without ever naming another tool.
func ReservedToolName(name string) bool {
	lower, ok := asciiLower(name)
	return ok && builtinTools[lower]
}

// CanonicalTool is the tool name a rule's name stands for: a built-in name in
// lower case, whatever case it was written in. Other names, MCP tools
// (mcp__server__tool), knowledge-base and extension tools, are compared as
// written, since their protocols or configuration tell one case from another.
func CanonicalTool(name string) string {
	if ReservedToolName(name) {
		lower, _ := asciiLower(name)
		return lower
	}
	return name
}

// asciiLower lowers an ASCII name; a name with any other character is not
// folded, so a look-alike letter never stands for a built-in name.
func asciiLower(name string) (string, bool) {
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			return name, false
		}
	}
	return strings.ToLower(name), true
}

// familiarTools are other harnesses' names for Abhed's tools, for suggestions.
var familiarTools = map[string]string{
	"multiedit": "edit", "webfetch": "web_fetch", "websearch": "web_search", "todowrite": "todo",
	"ls": "glob", "notebookedit": "edit", "shell": "bash",
}

// SuggestTool is the built-in name a misspelt or familiar one probably meant, or "".
func SuggestTool(name string) string {
	lower := strings.ToLower(name)
	if t, ok := familiarTools[lower]; ok {
		return t
	}
	best, bestD := "", 3
	for _, n := range BuiltinTools() {
		if d := editDistance(lower, n); d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
