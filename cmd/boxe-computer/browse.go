package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// browse (github.com/anomalyco/browse) drives the shared Chrome on the
// desktop through its extension. browse-mcp runs its MCP server for an
// agent, and starts the desktop, and with it Chrome, before a tool call
// finds browse without its browser: the desktop still starts only when used.
var browseBin = "/usr/local/bin/browse"

// browseTimeout is how long a first call waits for Chrome: on the VM's
// first start Chrome installs the extension and browse pairs with it.
const browseTimeout = 120 * time.Second

func serveBrowseMCP(stdin io.Reader, stdout, stderr io.Writer, ensure func() error) error {
	cmd := exec.Command(browseBin, "mcp")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	to, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	err = forwardMCP(stdin, to, stderr, ensure)
	to.Close()
	return errors.Join(err, cmd.Wait())
}

// forwardMCP copies the agent's messages, one JSON object per line, to
// browse, running ensure before each tool call. A failed ensure is only
// logged: browse then answers the call with what is wrong.
func forwardMCP(from io.Reader, to, log io.Writer, ensure func() error) error {
	r := bufio.NewReader(from)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if isToolCall(line) {
				if err := ensure(); err != nil {
					fmt.Fprintf(log, "boxe-computer: %v\n", err)
				}
			}
			if _, err := to.Write(line); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func isToolCall(line []byte) bool {
	var m struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(line, &m) == nil && m.Method == "tools/call"
}

// ensureBrowser starts the desktop and its Chrome if browse has no browser,
// and waits until the extension is connected to browse.
func ensureBrowser() error {
	if browseConnected() {
		return nil
	}
	if out, err := exec.Command("sudo", "-n", "systemctl", "start", "boxe-desktop", "boxe-chrome").CombinedOutput(); err != nil {
		return fmt.Errorf("cannot start the desktop: %s", firstLine(out, err))
	}
	// browse's relay, which the extension connects to, starts with any of
	// its commands but status.
	exec.Command(browseBin, "session", "list").Run()
	deadline := time.Now().Add(browseTimeout)
	for !browseConnected() {
		if time.Now().After(deadline) {
			return fmt.Errorf("Chrome's browse extension did not connect within %v", browseTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

func browseConnected() bool {
	out, err := exec.Command(browseBin, "status", "--json").Output()
	var s struct {
		Extension struct {
			PairingState string `json:"pairingState"`
		} `json:"extension"`
	}
	return err == nil && json.Unmarshal(out, &s) == nil && s.Extension.PairingState == "connected"
}
