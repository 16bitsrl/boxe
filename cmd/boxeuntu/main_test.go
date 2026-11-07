package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxe/cmd/boxeuntu/internal/agentconfig"
)

// An installation without a models' gateway still gives the agents the
// computer tools.
func TestConfigureWithoutAGateway(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	gatewayFile, computerBin, cli := agentconfig.GatewayFile, agentconfig.ComputerBin, claudeCLI
	t.Cleanup(func() { agentconfig.GatewayFile, agentconfig.ComputerBin, claudeCLI = gatewayFile, computerBin, cli })
	agentconfig.GatewayFile = filepath.Join(dir, "llm-gateway")
	agentconfig.ComputerBin = filepath.Join(dir, "boxe-computer")

	var calls []string
	claudeCLI = func(context.Context) func(args ...string) error {
		return func(args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			if args[1] == "get" {
				return errors.New("exit status 1")
			}
			return nil
		}
	}

	// No boxe-computer either (an older image): nothing to do.
	var out strings.Builder
	if err := run([]string{"configure"}, &out, 1000); err != nil || !strings.Contains(out.String(), "nothing to do") || calls != nil {
		t.Fatalf("without boxe-computer: %v\n%s", err, out.String())
	}

	os.WriteFile(agentconfig.ComputerBin, nil, 0o755)
	out.Reset()
	if err := run([]string{"configure"}, &out, 1000); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"claude: added the computer tools",
		"codex: written " + filepath.Join(home, ".codex/config.toml"),
		"opencode: written " + filepath.Join(home, ".config/opencode/opencode.json"),
		"pi: written " + filepath.Join(home, ".pi/agent/mcp.json"),
	} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("no %q in:\n%s", line, out.String())
		}
	}
	if strings.Contains(out.String(), "no key") {
		t.Errorf("keys spoken of without a gateway:\n%s", out.String())
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[1], "mcp add --scope user computer -- "+agentconfig.ComputerBin) {
		t.Fatalf("claude calls %q", calls)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if !strings.Contains(string(b), "[mcp_servers.computer]") || strings.Contains(string(b), "model_provider") {
		t.Fatalf("codex config:\n%s", b)
	}
}

// A gateway that does not answer leaves the files with models alone, but
// still gives the computer tools to the agents without a file of ours.
func TestConfigureWithTheGatewayDown(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	gatewayFile, computerBin, cli := agentconfig.GatewayFile, agentconfig.ComputerBin, claudeCLI
	t.Cleanup(func() { agentconfig.GatewayFile, agentconfig.ComputerBin, claudeCLI = gatewayFile, computerBin, cli })
	agentconfig.GatewayFile = filepath.Join(dir, "llm-gateway")
	agentconfig.ComputerBin = filepath.Join(dir, "boxe-computer")
	claudeCLI = func(context.Context) func(args ...string) error { return nil }
	os.WriteFile(agentconfig.ComputerBin, nil, 0o755)
	os.WriteFile(agentconfig.GatewayFile, []byte("http://127.0.0.1:1\n"), 0o644)
	codex := filepath.Join(home, ".codex/config.toml")
	os.MkdirAll(filepath.Dir(codex), 0o700)
	os.WriteFile(codex, []byte("# with models\n"), 0o600)

	var out strings.Builder
	if err := run([]string{"configure"}, &out, 1000); err == nil {
		t.Fatal("no error with the gateway down")
	}
	if b, _ := os.ReadFile(codex); string(b) != "# with models\n" {
		t.Errorf("codex's file changed: %q", b)
	}
	for _, f := range []string{".pi/agent/mcp.json", ".config/opencode/opencode.json"} {
		if _, err := os.Stat(filepath.Join(home, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
