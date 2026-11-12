// boxe-computer drives the desktop of a boxe VM, the X display :1 that the
// VM's desktop page shows: screenshots, mouse and keyboard, from the command
// line or as an MCP server for coding agents.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

const usage = `boxe-computer drives this VM's desktop (DISPLAY=:1, what the desktop page shows).

  boxe-computer [--json] <action> [args]

  screenshot [--full] [--out FILE]      the screen, scaled to fit 1280x800 unless --full
  zoom X0 Y0 X1 Y1 [--out FILE]         a region at full resolution
  left_click [X Y] [--mod MODS]         also right_click, middle_click, double_click,
                                        triple_click; MODS such as ctrl or ctrl+shift
  left_click_drag X0 Y0 X1 Y1
  mouse_move X Y
  left_mouse_down, left_mouse_up
  cursor_position                       prints "X Y"
  scroll up|down|left|right [N] [X Y] [--mod MODS]
                                        N notches, 3 if not given, at most 50
  type TEXT...                          the rest of the line is the text; "type -" reads it
                                        from stdin
  key COMBO... [--repeat N]             such as Return, ctrl+s, alt+Tab
  hold_key COMBO SECONDS
  wait SECONDS
  mcp                                   MCP server on stdin and stdout, one tool: computer
  browse-mcp                            browse's MCP server, for the Chrome on the desktop;
                                        starts the desktop when a tool call needs it

Coordinates are pixels of the last screenshot: take one first. The desktop
starts by itself. Without --out or --json, screenshot and zoom write the PNG
to stdout, which must not be a terminal (boxe-computer screenshot > s.png).
--json prints one JSON line, with the PNG in base64.
`

func main() {
	if os.Getenv("DISPLAY") == "" {
		os.Setenv("DISPLAY", ":1")
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, newComputer()))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, c *computer) int {
	asJSON := false
	for len(args) > 0 && args[0] == "--json" {
		asJSON, args = true, args[1:]
	}
	if len(args) > 0 {
		switch args[0] {
		case "help", "--help", "-h":
			fmt.Fprint(stdout, usage)
			return 0
		case "mcp":
			if len(args) > 1 {
				return fail(stderr, usagef("mcp takes no arguments"))
			}
			// An agent's screenshots are its own coordinate space: a
			// screenshot taken meanwhile with the command must not move it.
			c.statePath = ""
			return fail(stderr, serveMCP(c, stdin, stdout))
		case "browse-mcp":
			if len(args) > 1 {
				return fail(stderr, usagef("browse-mcp takes no arguments"))
			}
			return fail(stderr, serveBrowseMCP(stdin, stdout, stderr, ensureBrowser))
		}
	}
	r, err := parseArgs(args, stdin)
	if err != nil {
		return fail(stderr, err)
	}
	for _, a := range args[1:] {
		if a == "--json" && r.action != "type" {
			asJSON = true
		}
	}
	if (r.action == "screenshot" || r.action == "zoom") && !asJSON && r.out == "" && isTerminal(stdout) {
		return fail(stderr, usagef("stdout is a terminal: use --out FILE or redirect it to a file"))
	}
	done, err := c.turn(r)
	if err != nil {
		return fail(stderr, err)
	}
	res, err := c.do(r)
	done()
	if err != nil {
		return fail(stderr, err)
	}
	if res.png != nil && r.out != "" {
		if err := os.WriteFile(r.out, res.png, 0o644); err != nil {
			return fail(stderr, err)
		}
	}
	return fail(stderr, output(stdout, r, res, asJSON))
}

func output(w io.Writer, r *request, res *result, asJSON bool) error {
	switch {
	case asJSON && res.png != nil:
		return writeJSON(w, struct {
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			ScreenWidth  int    `json:"screen_width"`
			ScreenHeight int    `json:"screen_height"`
			PNG          string `json:"png"`
		}{res.width, res.height, res.screenW, res.screenH, base64.StdEncoding.EncodeToString(res.png)})
	case asJSON && res.cursor != nil:
		return writeJSON(w, map[string]int{"x": res.cursor.x, "y": res.cursor.y})
	case asJSON:
		return writeJSON(w, map[string]string{"action": r.action, "status": "done"})
	case res.png != nil && r.out != "":
		_, err := fmt.Fprintln(w, res.sizes())
		return err
	case res.png != nil:
		_, err := w.Write(res.png)
		return err
	case res.cursor != nil:
		_, err := fmt.Fprintf(w, "%d %d\n", res.cursor.x, res.cursor.y)
		return err
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// fail reports err and returns the exit status: 0 without an error, 2 for
// usage errors, 1 for the rest.
func fail(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, "boxe-computer:", err)
	if errors.As(err, new(usageError)) {
		return 2
	}
	return 1
}
