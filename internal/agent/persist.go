package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/dxn111/EVOLV/internal/providers"
	"github.com/dxn111/EVOLV/internal/redact"
	"github.com/dxn111/EVOLV/internal/workspace"
)

// The mind keeps a private notebook: the conversation survives restarts.
// Stored as plain JSON in the genome (workspace/conversation.json) so the
// user can read it, and the agent could even evolve how it persists itself.

// memoryFile is the workspace-relative path of the persisted conversation.
func (a *Agent) memoryFile() string {
	if a.profile != nil && a.profile.ID != "" {
		dir := filepath.Join(a.ws.Dir(workspace.WorkspaceDir), "agents", a.profile.ID)
		_ = os.MkdirAll(dir, 0o755)
		return filepath.Join(dir, "conversation.json")
	}
	return filepath.Join(a.ws.Dir(workspace.WorkspaceDir), "conversation.json")
}

// saveMemory writes the current history to disk (bounded, same trim as RAM).
// Credentials and API keys are redacted before writing to disk.
func (a *Agent) saveMemory() {
	a.mu.Lock()
	hist := a.history
	a.mu.Unlock()
	if len(hist) == 0 {
		_ = os.Remove(a.memoryFile())
		return
	}
	safe := make([]providers.Message, len(hist))
	for i, m := range hist {
		safe[i] = providers.Message{
			Role:    m.Role,
			Content: sanitizePersistedContent(m.Content),
		}
	}
	raw, err := json.Marshal(safe)
	if err != nil {
		return
	}
	tmp := a.memoryFile() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, a.memoryFile()) // atomic: a crash never tears memory
}

func sanitizePersistedContent(content string) string {
	if strings.HasPrefix(content, "kernel:") {
		payload := strings.TrimPrefix(content, "kernel:")
		var decoded any
		if json.Unmarshal([]byte(payload), &decoded) == nil {
			if raw, err := json.Marshal(redact.RedactAny(decoded)); err == nil {
				return "kernel:" + string(raw)
			}
		}
	}
	return redact.Redact(content)
}

// loadMemory restores a persisted conversation at boot, if any.
func (a *Agent) loadMemory() {
	raw, err := os.ReadFile(a.memoryFile())
	if err != nil {
		return
	}
	var hist []providers.Message
	if json.Unmarshal(raw, &hist) != nil || len(hist) == 0 {
		return
	}
	a.mu.Lock()
	a.history = hist
	a.mu.Unlock()
}
