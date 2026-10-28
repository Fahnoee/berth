package scan

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Fahnoee/berth/internal/proc"
)

// knownAgents maps executable names to a friendly agent name.
var knownAgents = map[string]string{
	"claude":       "claude",
	"claude-code":  "claude",
	"codex":        "codex",
	"aider":        "aider",
	"opencode":     "opencode",
	"gemini":       "gemini",
	"cursor-agent": "cursor",
	"amp":          "amp",
	"goose":        "goose",
	"crush":        "crush",
	"copilot":      "copilot",
	"qwen":         "qwen",
	"kiro":         "kiro",
	"droid":        "droid",
}

// substrings found in interpreter-launched agents ("node .../claude-code/cli.js").
var agentHints = map[string]string{
	"@anthropic-ai/claude-code": "claude",
	"@openai/codex":             "codex",
	"@google/gemini-cli":        "gemini",
	"opencode-ai":               "opencode",
}

func init() {
	for _, n := range strings.Split(os.Getenv("BERTH_AGENTS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			knownAgents[n] = n
		}
	}
}

// AgentName returns the agent a process is, or "".
func AgentName(p proc.Info) string {
	cands := []string{p.Name, p.Exe()}
	if len(p.Cmdline) > 1 {
		cands = append(cands, filepath.Base(p.Cmdline[1]))
	}
	for _, c := range cands {
		if a, ok := knownAgents[strings.ToLower(c)]; ok {
			return a
		}
	}
	cmd := p.Command()
	for hint, a := range agentHints {
		if strings.Contains(cmd, hint) {
			return a
		}
	}
	return ""
}

// FindAgent walks up from pid and returns the nearest agent ancestor.
func FindAgent(pid int) (proc.Info, string, bool) {
	for _, a := range proc.Ancestors(pid) {
		if name := AgentName(a); name != "" {
			return a, name, true
		}
	}
	return proc.Info{}, "", false
}
