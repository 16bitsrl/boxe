// boxeuntu manages the coding agents of a boxe VM: installs and updates
// them, and sets them up for the boxe LLM gateway. Forked from exeuntu's
// CLI (Bold Software, Apache 2.0): see NOTICE.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"boxe/cmd/boxeuntu/internal/agentconfig"
	"boxe/cmd/boxeuntu/internal/agentupdate"
	"boxe/cmd/boxeuntu/internal/piupdate"
)

var version = "dev"

const usage = `boxeuntu manages the coding agents of this VM.

  boxeuntu configure                 set Codex, OpenCode and pi up for the models of your
                                     boxe account (Claude Code needs nothing), and give
                                     every agent the computer tools (MCP server "computer");
                                     run it after ssh boxe llm set, or restart the VM
  sudo boxeuntu update <agent>       update claude, codex or opencode to the latest version
  boxeuntu update pi                 pi lives in your home: without sudo
  boxeuntu install <agent>           the same, for an agent that is not there
  boxeuntu version

  --version <v> after the agent installs that version instead of the latest.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1:], os.Stdout, os.Geteuid()); err != nil {
		fmt.Fprintln(os.Stderr, "boxeuntu:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer, euid int) error {
	switch args[0] {
	case "configure":
		return configure(args[1:], stdout)
	case "install", "update":
		return install(args[0], args[1:], stdout, euid)
	case "version", "--version":
		fmt.Fprintln(stdout, "boxeuntu", version)
		return nil
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (boxeuntu help)", args[0])
}

func configure(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	gateway := fs.String("gateway", "", "the gateway's URL (default: from "+agentconfig.GatewayFile+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, err := os.Stat(agentconfig.ComputerBin)
	computer := err == nil
	if *gateway == "" {
		b, err := os.ReadFile(agentconfig.GatewayFile)
		switch {
		case errors.Is(err, os.ErrNotExist) && !computer:
			fmt.Fprintln(stdout, "no models' gateway on this boxe installation: nothing to do")
			return nil
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintln(stdout, "no models' gateway on this boxe installation: only the computer tools")
		case err != nil:
			return err
		default:
			*gateway = strings.TrimSpace(string(b))
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	unlock, err := agentconfig.Lock(home)
	if err != nil {
		return err
	}
	defer unlock()
	// Claude Code first: a gateway that does not answer must not keep it
	// from the computer tools.
	var claudeErr error
	if computer {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		status, err := agentconfig.AddClaudeComputer(home, claudeCLI(ctx))
		cancel()
		switch {
		case err != nil:
			claudeErr = fmt.Errorf("claude: %w", err)
		case status == "added":
			fmt.Fprintln(stdout, "claude: added the computer tools")
		case status == "there":
			fmt.Fprintln(stdout, "claude: has the computer tools already")
		}
	}
	var g agentconfig.Gateway
	if *gateway != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if g, err = agentconfig.Discover(ctx, &http.Client{}, *gateway); err != nil {
			// Still give the computer tools to who has no file of ours yet:
			// rewriting the others now would drop their models.
			if computer {
				var safe []agentconfig.File
				for _, f := range agentconfig.Files(agentconfig.Gateway{}, true) {
					if _, serr := os.Stat(filepath.Join(home, f.Path)); errors.Is(serr, os.ErrNotExist) {
						safe = append(safe, f)
					}
				}
				_, aerr := agentconfig.Apply(home, safe)
				return errors.Join(claudeErr, err, aerr)
			}
			return errors.Join(claudeErr, err)
		}
	}
	results, err := agentconfig.Apply(home, agentconfig.Files(g, computer))
	for _, r := range results {
		switch r.Status {
		case "written", "removed", "unchanged":
			fmt.Fprintf(stdout, "%s: %s %s\n", r.Agent, r.Status, r.Path)
		case "kept":
			fmt.Fprintf(stdout, "%s: %s is your own, left as it is\n", r.Agent, r.Path)
		case "none":
			if g.URL == "" {
				continue // no gateway: no keys to speak of
			}
			fmt.Fprintf(stdout, "%s: no key for it in your boxe account (ssh boxe llm set)\n", r.Agent)
		}
	}
	return errors.Join(claudeErr, err)
}

// claudeCLI runs Claude Code's CLI, or is nil when it is not installed. A
// variable for tests.
var claudeCLI = func(ctx context.Context) func(args ...string) error {
	path, err := exec.LookPath("claude")
	if err != nil {
		if _, err := os.Stat("/usr/local/bin/claude"); err != nil {
			return nil
		}
		path = "/usr/local/bin/claude"
	}
	return func(args ...string) error {
		out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

func install(cmd string, args []string, stdout io.Writer, euid int) error {
	if len(args) == 0 {
		return fmt.Errorf("%s which agent? claude, codex, opencode or pi", cmd)
	}
	agent := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	v := fs.String("version", "", "version to install instead of the latest")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected %q", fs.Arg(0))
	}
	out := stdout
	if agent == "pi" {
		if euid == 0 {
			return errors.New("pi is installed in your home: run it without sudo")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := piupdate.Update(ctx, piupdate.Options{Version: *v, Stdout: out})
		return err
	}
	switch a := agentupdate.Agent(agent); a {
	case agentupdate.AgentClaude, agentupdate.AgentCodex, agentupdate.AgentOpenCode:
		if euid != 0 {
			return fmt.Errorf("%s is installed in /usr/local/bin: run sudo boxeuntu %s %s", agent, cmd, agent)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_, err := agentupdate.Update(ctx, agentupdate.Options{Agent: a, Version: *v, Stdout: out})
		return err
	}
	return fmt.Errorf("unknown agent %q: claude, codex, opencode or pi", agent)
}
