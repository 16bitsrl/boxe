package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const toolDescription = `Drives the Linux desktop of this VM (DISPLAY=:1): the screen a person sees on the VM's desktop page. ` +
	`Take a screenshot first: coordinates are pixels of the last screenshot, which is scaled to fit 1280x800. ` +
	`Mouse and keyboard actions return a new screenshot. The desktop starts by itself.`

// settle is how long actions wait before the screenshot they return, so the
// screen shows their effect.
const settle = 500 * time.Millisecond

func toolSchema() map[string]any {
	intPair := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "minItems": 2, "maxItems": 2, "description": desc}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":           map[string]any{"type": "string", "enum": actions},
			"coordinate":       intPair("[x, y] in the last screenshot: where to click, move, scroll or end a drag"),
			"start_coordinate": intPair("[x, y] where left_click_drag starts"),
			"text":             map[string]any{"type": "string", "description": "the text for type; the key or combo for key and hold_key, such as Return or ctrl+s; modifiers held during clicks and scroll, such as shift or ctrl+shift"},
			"scroll_direction": map[string]any{"type": "string", "enum": []string{"up", "down", "left", "right"}},
			"scroll_amount":    map[string]any{"type": "integer", "minimum": 1, "maximum": maxScroll, "description": "notches to scroll, 3 if not given"},
			"duration":         map[string]any{"type": "number", "minimum": 0, "maximum": maxSeconds, "description": "seconds, for wait and hold_key"},
			"region":           map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "minItems": 4, "maxItems": 4, "description": "[x0, y0, x1, y1] in the last screenshot, for zoom"},
			"repeat":           map[string]any{"type": "integer", "minimum": 1, "maximum": maxRepeat, "description": "how many times key presses the keys"},
		},
		"required": []string{"action"},
	}
}

type toolInput struct {
	Action          string    `json:"action"`
	Coordinate      []float64 `json:"coordinate"`
	StartCoordinate []float64 `json:"start_coordinate"`
	Text            *string   `json:"text"`
	ScrollDirection string    `json:"scroll_direction"`
	ScrollAmount    *float64  `json:"scroll_amount"`
	Duration        *float64  `json:"duration"`
	Region          []float64 `json:"region"`
	Repeat          *float64  `json:"repeat"`
}

func serveMCP(c *computer, stdin io.Reader, stdout io.Writer) error {
	s := newMCPServer(c)
	return s.Run(context.Background(), &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func newMCPServer(c *computer) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "boxe-computer", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "computer", Description: toolDescription, InputSchema: toolSchema()},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return callTool(c, req.Params.Arguments), nil
		})
	return s
}

func callTool(c *computer, args json.RawMessage) *mcp.CallToolResult {
	var in toolInput
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return toolError(fmt.Errorf("bad arguments: %v", err))
	}
	r, err := in.request()
	if err != nil {
		return toolError(err)
	}
	// The turn lasts until the screenshot that shows the action.
	done, err := c.turn(r)
	if err != nil {
		return toolError(err)
	}
	defer done()
	res, err := c.do(r)
	if err != nil {
		return toolError(err)
	}
	switch r.action {
	case "screenshot", "zoom":
	case "cursor_position":
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%d %d", res.cursor.x, res.cursor.y)}}}
	default:
		if r.action != "wait" {
			c.sleep(settle)
		}
		s, err := c.open()
		if err != nil {
			return toolError(fmt.Errorf("%s done, but no screenshot: %v", r.action, err))
		}
		defer s.Close()
		if res, err = c.screenshot(s, false); err != nil {
			return toolError(fmt.Errorf("%s done, but no screenshot: %v", r.action, err))
		}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: res.png, MIMEType: "image/png"},
		&mcp.TextContent{Text: res.sizes()},
	}}
}

func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

// request validates the tool's input as the command line does.
func (in toolInput) request() (*request, error) {
	action, ok := canonicalAction(in.Action)
	if !ok {
		return nil, fmt.Errorf("unknown action %q", in.Action)
	}
	r := &request{action: action, repeat: 1}
	text := ""
	if in.Text != nil {
		text = *in.Text
	}
	var err error
	need := func(what string, ok bool) {
		if err == nil && !ok {
			err = fmt.Errorf("%s needs %s", action, what)
		}
	}
	switch {
	case action == "zoom":
		need("region", in.Region != nil)
		if err == nil {
			var g []int
			if g, err = ints("region", in.Region, 4); err == nil {
				copy(r.region[:], g)
				err = checkRegion(r.region)
			}
		}
	case isClick(action):
		if in.Coordinate != nil {
			r.at, err = pointArg("coordinate", in.Coordinate)
		}
		if err == nil {
			r.mods, err = parseMods(text)
		}
	case action == "mouse_move":
		need("coordinate", in.Coordinate != nil)
		if err == nil {
			r.at, err = pointArg("coordinate", in.Coordinate)
		}
	case action == "left_click_drag":
		need("start_coordinate and coordinate", in.StartCoordinate != nil && in.Coordinate != nil)
		if err == nil {
			if r.from, err = pointArg("start_coordinate", in.StartCoordinate); err == nil {
				r.at, err = pointArg("coordinate", in.Coordinate)
			}
		}
	case action == "scroll":
		r.dir, r.amount = strings.ToLower(in.ScrollDirection), 3
		need("scroll_direction", r.dir != "")
		if err == nil {
			err = checkDir(r.dir)
		}
		if err == nil && in.ScrollAmount != nil {
			r.amount, err = intRange("scroll_amount", *in.ScrollAmount, 1, maxScroll)
		}
		if err == nil && in.Coordinate != nil {
			r.at, err = pointArg("coordinate", in.Coordinate)
		}
		if err == nil {
			r.mods, err = parseMods(text)
		}
	case action == "type":
		need("text", text != "")
		if err == nil && len(text) > maxTypeText {
			err = fmt.Errorf("the text is longer than 8 KiB")
		}
		r.text = text
	case action == "key", action == "hold_key":
		need("text, such as Return or ctrl+s", strings.TrimSpace(text) != "")
		if err == nil {
			r.keys, err = parseKeys(text)
		}
		if err == nil && action == "key" && in.Repeat != nil {
			r.repeat, err = intRange("repeat", *in.Repeat, 1, maxRepeat)
		}
		if err == nil && action == "hold_key" {
			need("duration", in.Duration != nil)
			if err == nil {
				r.seconds, err = *in.Duration, checkSeconds(*in.Duration)
			}
		}
	case action == "wait":
		need("duration", in.Duration != nil)
		if err == nil {
			r.seconds, err = *in.Duration, checkSeconds(*in.Duration)
		}
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func ints(what string, v []float64, n int) ([]int, error) {
	if len(v) != n {
		return nil, fmt.Errorf("%s needs %d numbers, not %d", what, n, len(v))
	}
	out := make([]int, n)
	for i, f := range v {
		if f != math.Trunc(f) || math.Abs(f) > 1e6 {
			return nil, fmt.Errorf("%s holds whole numbers of pixels, not %v", what, f)
		}
		out[i] = int(f)
	}
	return out, nil
}

func pointArg(what string, v []float64) (*point, error) {
	p, err := ints(what, v, 2)
	if err != nil {
		return nil, err
	}
	return &point{p[0], p[1]}, nil
}

func intRange(what string, f float64, lo, hi int) (int, error) {
	if f != math.Trunc(f) || f < float64(lo) || f > float64(hi) {
		return 0, fmt.Errorf("%s is a whole number from %d to %d, not %v", what, lo, hi, f)
	}
	return int(f), nil
}
