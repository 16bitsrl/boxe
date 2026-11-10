package agentconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// claudeSettingsPath is Claude Code's user settings, also changed by
// Claude Code itself: we only set the variables of its "env".
const claudeSettingsPath = ".claude/settings.json"

// claudeEnvState keeps the variables we last set there, with their values.
const claudeEnvState = ".config/boxeuntu/claude-env.json"

// claudeEnv are the variables that have Claude Code use the owner's Claude
// subscription through the gateway, in every shell and service: its
// placeholder token, and no API key, which Claude Code would prefer.
// Programs it runs get them too: their placeholder-less API calls get the
// owner's API key from the gateway, as before.
func claudeEnv(g Gateway) map[string]string {
	if !g.Claude || g.URL == "" {
		return nil
	}
	return map[string]string{
		"ANTHROPIC_BASE_URL":      g.URL + "/anthropic",
		"CLAUDE_CODE_OAUTH_TOKEN": placeholder,
		"ANTHROPIC_API_KEY":       "",
	}
}

// ClaudeSettings sets the variables of g's Claude subscription in Claude
// Code's settings, or takes them away without it. A variable the user set
// to a value of their own is left alone, and so is a file that is not JSON.
// It says "written", "removed", "unchanged", "kept" or "none".
func ClaudeSettings(home string, g Gateway) (string, error) {
	want := claudeEnv(g)
	ours := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(home, claudeEnvState)); err == nil {
		_ = json.Unmarshal(b, &ours) // a broken state only means nothing is ours
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	path := filepath.Join(home, claudeSettingsPath)
	settings := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if len(want) == 0 {
			return "none", writeState(home, map[string]string{})
		}
	case err != nil:
		return "", err
	default:
		if json.Unmarshal(b, &settings) != nil || settings == nil {
			return "kept", nil
		}
	}
	env, ok := settings["env"].(map[string]any)
	if !ok {
		if settings["env"] != nil {
			return "kept", nil
		}
		env = map[string]any{}
	}
	added, removed := false, false
	next := map[string]string{}
	for k, v := range want {
		cur, has := env[k]
		prev, mine := ours[k]
		switch {
		case has && !(mine && cur == prev):
			// The user's own value.
		case has && cur == v:
			next[k] = v
		default:
			env[k] = v
			next[k] = v
			added = true
		}
	}
	for k, prev := range ours {
		if _, still := want[k]; still {
			continue
		}
		if cur, has := env[k]; has && cur == prev {
			delete(env, k)
			removed = true
		}
	}
	if !added && !removed {
		return map[bool]string{true: "unchanged", false: "none"}[len(next) > 0], writeState(home, next)
	}
	if len(env) == 0 {
		delete(settings, "env")
	} else {
		settings["env"] = env
	}
	if err := writeFile(path, jsonFile(settings)); err != nil {
		return "", err
	}
	status := "written"
	if !added {
		status = "removed"
	}
	return status, writeState(home, next)
}

func writeState(home string, state map[string]string) error {
	b, _ := json.MarshalIndent(state, "", "  ")
	return writeFile(filepath.Join(home, claudeEnvState), append(b, '\n'))
}
