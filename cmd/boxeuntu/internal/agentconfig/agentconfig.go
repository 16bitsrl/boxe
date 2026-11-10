// Package agentconfig sets the coding agents of a VM up for the boxe LLM
// gateway: the host adds the owner's API key to their requests. Claude
// Code needs nothing for it: it reads ANTHROPIC_BASE_URL, which login
// shells get. Codex, OpenCode and pi do not, so they get a config file
// each, for the providers the owner has a key for. The owner's
// subscriptions go to Codex (its config file) and Claude Code (claude.go).
//
// Every agent also gets the computer tools (the MCP server "computer",
// boxe-computer), with or without a gateway.
//
// A file is ours while it holds what we last wrote (its hash is kept in a
// state file): once the user changes it, it is theirs and left alone.
package agentconfig

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// GatewayFile holds the gateway's URL, written by the host in every VM.
var GatewayFile = "/etc/boxe/llm-gateway"

// ComputerBin drives the VM's desktop: the agents run it as the MCP server
// "computer". Images older than it do not have it.
var ComputerBin = "/usr/local/bin/boxe-computer"

// The models OpenCode starts with: it has no default of its own for them.
const (
	openCodeAnthropicModel = "anthropic/claude-opus-5-5"
	openCodeOpenAIModel    = "openai/gpt-6.1-sol"
)

// placeholder is the key agents send: the gateway puts the owner's in its place.
const placeholder = "boxe"

// Gateway is what the gateway offers this VM.
type Gateway struct {
	URL       string // e.g. http://10.42.0.1:2998
	Anthropic bool   // the owner has a key for the provider
	OpenAI    bool
	Claude    bool // the owner has a subscription, for Claude Code
	ChatGPT   bool // the same, for Codex
}

// Discover asks the gateway at url which providers the VM's owner has a key
// for. It answers with the variables of login shells, one export per line.
func Discover(ctx context.Context, client *http.Client, url string) (Gateway, error) {
	url = strings.TrimRight(url, "/")
	g := Gateway{URL: url}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/env", nil)
	if err != nil {
		return g, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return g, fmt.Errorf("boxe gateway: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return g, fmt.Errorf("boxe gateway: %s", resp.Status)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<16))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "export ANTHROPIC_API_KEY="):
			g.Anthropic = true
		case strings.HasPrefix(line, "export OPENAI_API_KEY="):
			g.OpenAI = true
		case strings.HasPrefix(line, "export BOXE_LLM_PLANS="):
			for _, plan := range strings.Fields(strings.Trim(strings.TrimPrefix(line, "export BOXE_LLM_PLANS="), `"`)) {
				g.Claude = g.Claude || plan == "claude"
				g.ChatGPT = g.ChatGPT || plan == "chatgpt"
			}
		}
	}
	return g, sc.Err()
}

// File is one agent's config file, relative to the home directory. Content
// nil means the file should not be there.
type File struct {
	Agent   string
	Path    string
	Content []byte
	Exec    bool // a script
}

// ClaudeSubscriptionScript runs Claude Code for OpenCode's Claude Agent
// provider, which does not read ~/.claude/settings.json: it drops the API
// key of the login shell, which would win over the subscription. The
// gateway points OpenCode at it (OPENCODE_CLAUDE_PATH) when it is there.
const ClaudeSubscriptionScript = ".config/boxeuntu/claude-subscription"

func claudeSubscriptionScript(g Gateway) []byte {
	if !g.Claude {
		return nil
	}
	return []byte("#!/bin/sh\n# Written by boxeuntu: Claude Code for OpenCode, on your Claude subscription.\nunset ANTHROPIC_API_KEY\nexec claude \"$@\"\n")
}

// Files are the config files for g, with the computer tools if computer.
// A zero g (no gateway) gives the computer tools alone.
func Files(g Gateway, computer bool) []File {
	files := []File{
		{"codex", ".codex/config.toml", codexConfig(g, computer), false},
		{"opencode", ".config/opencode/opencode.json", openCodeConfig(g, computer), false},
		{"pi", ".pi/agent/models.json", piModels(g), false},
		{"claude", ClaudeSubscriptionScript, claudeSubscriptionScript(g), true},
	}
	if computer {
		// pi keeps its MCP servers in a file of their own.
		files = append(files, File{"pi", ".pi/agent/mcp.json", jsonFile(map[string]any{
			"mcpServers": map[string]any{"computer": map[string]any{"command": ComputerBin, "args": []string{"mcp"}}},
		}), false})
	}
	return files
}

func codexConfig(g Gateway, computer bool) []byte {
	if !g.OpenAI && !g.ChatGPT && !computer {
		return nil
	}
	var b strings.Builder
	b.WriteString("# Written by boxeuntu. Change this file and boxeuntu leaves it alone.\n")
	switch {
	case g.ChatGPT:
		b.WriteString(`
# Your ChatGPT subscription, through the boxe gateway, which signs you in.
model_provider = "boxe"

[model_providers.boxe]
name = "boxe"
base_url = "` + g.URL + `/chatgpt"
wire_api = "responses"
experimental_bearer_token = "` + placeholder + `"
`)
	case g.OpenAI:
		b.WriteString(`
# OpenAI models through the boxe gateway, which adds your key.
model_provider = "boxe"

[model_providers.boxe]
name = "boxe"
base_url = "` + g.URL + `/openai"
requires_openai_auth = false
`)
	}
	if computer {
		fmt.Fprintf(&b, `
# The computer tools: screen, mouse and keyboard of this VM's desktop.
[mcp_servers.computer]
command = %q
args = ["mcp"]
`, ComputerBin)
	}
	return []byte(b.String())
}

func openCodeConfig(g Gateway, computer bool) []byte {
	providers := map[string]any{}
	model := ""
	if g.OpenAI {
		providers["openai"] = map[string]any{"options": map[string]string{"baseURL": g.URL + "/openai", "apiKey": placeholder}}
		model = openCodeOpenAIModel
	}
	if g.Anthropic {
		providers["anthropic"] = map[string]any{"options": map[string]string{"baseURL": g.URL + "/anthropic/v1", "apiKey": placeholder}}
		model = openCodeAnthropicModel
	}
	if len(providers) == 0 && !computer {
		return nil
	}
	config := map[string]any{"$schema": "https://opencode.ai/config.json"}
	if len(providers) > 0 {
		config["model"] = model
		config["provider"] = providers
	}
	if computer {
		// The shape opencode mcp add writes since OpenCode 2.
		config["mcp"] = map[string]any{"servers": map[string]any{
			"computer": map[string]any{"type": "local", "command": []string{ComputerBin, "mcp"}},
		}}
	}
	return jsonFile(config)
}

func piModels(g Gateway) []byte {
	providers := map[string]any{}
	if g.Anthropic {
		providers["anthropic"] = map[string]string{"baseUrl": g.URL + "/anthropic", "apiKey": placeholder}
	}
	if g.OpenAI {
		providers["openai"] = map[string]string{"baseUrl": g.URL + "/openai", "apiKey": placeholder}
	}
	if len(providers) == 0 {
		return nil
	}
	return jsonFile(map[string]any{"providers": providers})
}

func jsonFile(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ") // maps of strings: cannot fail
	return append(b, '\n')
}

// Result says what Apply did with one file.
type Result struct {
	Agent, Path string
	Status      string // written, removed, unchanged, kept (the user's own), none
}

// Lock waits until no other run in home holds the lock, and takes it: the
// boot and the host may both run configure at once. Call the returned
// function to release it.
func Lock(home string) (func(), error) {
	path := filepath.Join(home, ".config/boxeuntu/lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// statePath keeps the hash of what we last wrote to each file.
const statePath = ".config/boxeuntu/agents.json"

// Apply writes files in home, removes the ones that should not be there,
// and leaves alone those the user changed.
func Apply(home string, files []File) ([]Result, error) {
	state := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(home, statePath)); err == nil {
		_ = json.Unmarshal(b, &state) // a broken state only means nothing is ours
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var results []Result
	for _, f := range files {
		path := filepath.Join(home, f.Path)
		res := Result{Agent: f.Agent, Path: path}
		current, err := os.ReadFile(path)
		exists := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return results, err
		}
		ours := exists && state[f.Path] != "" && state[f.Path] == hash(current)
		switch {
		case f.Content == nil && !exists:
			res.Status = "none"
			delete(state, f.Path)
		case f.Content == nil && ours:
			if err := os.Remove(path); err != nil {
				return results, err
			}
			res.Status = "removed"
			delete(state, f.Path)
		case exists && !ours && (f.Content == nil || hash(current) != hash(f.Content)):
			res.Status = "kept"
			delete(state, f.Path)
		case exists && hash(current) == hash(f.Content):
			res.Status = "unchanged"
			state[f.Path] = hash(f.Content)
		default:
			if err := writeFile(path, f.Content); err != nil {
				return results, err
			}
			if f.Exec {
				if err := os.Chmod(path, 0o700); err != nil {
					return results, err
				}
			}
			res.Status = "written"
			state[f.Path] = hash(f.Content)
		}
		results = append(results, res)
	}
	b, _ := json.MarshalIndent(state, "", "  ")
	return results, writeFile(filepath.Join(home, statePath), append(b, '\n'))
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeFile replaces path atomically, readable only by its user: the files
// name the gateway, and agents' configs may later hold the user's own keys.
func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// claudeMarker records that Claude Code was given the computer tools once:
// a user who removes them does not get them back.
const claudeMarker = ".config/boxeuntu/claude-computer"

// AddClaudeComputer gives Claude Code the computer tools, once. Its user
// config (~/.claude.json) is also its own state, so only its CLI changes it.
// claude runs that CLI with args, and is nil when Claude Code is not there.
// It says "added", "there" (the user had them already), or "" when there
// was nothing to do.
func AddClaudeComputer(home string, claude func(args ...string) error) (string, error) {
	marker := filepath.Join(home, claudeMarker)
	if _, err := os.Stat(marker); err == nil || claude == nil {
		return "", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	status := "there"
	if claude("mcp", "get", "computer") != nil {
		if err := claude("mcp", "add", "--scope", "user", "computer", "--", ComputerBin, "mcp"); err != nil {
			return "", err
		}
		status = "added"
	}
	return status, writeFile(marker, nil)
}
