package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// computer runs actions on the display. Its fields are replaced in tests.
type computer struct {
	open      func() (screen, error)
	xdo       func(args ...string) error
	sleep     func(time.Duration)
	statePath string     // the last screenshot's scale, shared by the user's commands
	lockPath  string     // held during each action, so actions take turns
	mu        sync.Mutex // the same, between the MCP server.s calls
	state     *shotState // or kept here, when statePath is empty
}

func newComputer() *computer {
	return &computer{open: openDisplay, xdo: runXdotool, sleep: time.Sleep, statePath: defaultStatePath(), lockPath: "/tmp/boxe-computer.lock"}
}

// result is what an action returns: an image, a cursor position, or nothing.
type result struct {
	png              []byte
	width, height    int // image
	screenW, screenH int
	cursor           *point
}

func (r *result) sizes() string {
	return fmt.Sprintf("screen %dx%d, image %dx%d", r.screenW, r.screenH, r.width, r.height)
}

func runXdotool(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xdotool", args...).CombinedOutput()
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("xdotool is not installed")
	}
	if err != nil {
		return fmt.Errorf("xdotool %s: %s", args[0], firstLine(out, err))
	}
	// xdotool warns about unknown key names and goes on with exit status 0.
	if s := string(out); strings.Contains(s, "No such key name") {
		return fmt.Errorf("xdotool %s: %s", args[0], firstLine(out, nil))
	}
	return nil
}

func (c *computer) do(r *request) (*result, error) {
	if r.action == "wait" {
		c.sleep(seconds(r.seconds))
		return &result{}, nil
	}
	s, err := c.open()
	if err != nil {
		return nil, err
	}
	defer s.Close()

	switch r.action {
	case "screenshot":
		return c.screenshot(s, r.full)
	case "zoom":
		return c.zoom(s, r.region)
	case "cursor_position":
		m, err := c.mapping(s)
		if err != nil {
			return nil, err
		}
		x, y, err := s.Pointer()
		if err != nil {
			return nil, err
		}
		p := m.toImage(point{x, y})
		return &result{cursor: &p}, nil
	case "type":
		return &result{}, c.typeText(s, r.text)
	case "key":
		args := append([]string{"key", "--repeat", strconv.Itoa(r.repeat), "--delay", "50", "--"}, r.keys...)
		return &result{}, c.xdo(args...)
	case "hold_key":
		up := append([]string{"keyup", "--"}, r.keys...)
		if err := c.xdo(append([]string{"keydown", "--"}, r.keys...)...); err != nil {
			c.xdo(up...) // keydown may have pressed some of them
			return nil, err
		}
		c.sleep(seconds(r.seconds))
		return &result{}, c.xdo(up...)
	case "left_mouse_down":
		return &result{}, c.xdo("mousedown", "1")
	case "left_mouse_up":
		return &result{}, c.xdo("mouseup", "1")
	}

	// The rest moves the pointer: map coordinates first, so a stale
	// screenshot stops the action before anything happens.
	m, err := c.mapping(s)
	if err != nil {
		return nil, err
	}
	var at, from point
	if r.at != nil {
		if at, err = m.toScreen(*r.at); err != nil {
			return nil, err
		}
	}
	if r.from != nil {
		if from, err = m.toScreen(*r.from); err != nil {
			return nil, err
		}
	}
	switch r.action {
	case "mouse_move":
		return &result{}, c.move(at)
	case "left_click_drag":
		if err := c.move(from); err != nil {
			return nil, err
		}
		if err := c.xdo("mousedown", "1"); err != nil {
			return nil, err
		}
		err := c.move(at)
		if uerr := c.xdo("mouseup", "1"); err == nil {
			err = uerr
		}
		return &result{}, err
	case "scroll":
		if r.at != nil {
			if err := c.move(at); err != nil {
				return nil, err
			}
		}
		button := map[string]string{"up": "4", "down": "5", "left": "6", "right": "7"}[r.dir]
		return &result{}, c.withMods(r.mods, func() error {
			return c.xdo("click", "--repeat", strconv.Itoa(r.amount), "--delay", "30", button)
		})
	}
	// Clicks.
	if r.at != nil {
		if err := c.move(at); err != nil {
			return nil, err
		}
	}
	args := []string{"click"}
	switch r.action {
	case "double_click":
		args = append(args, "--repeat", "2", "--delay", "80")
	case "triple_click":
		args = append(args, "--repeat", "3", "--delay", "80")
	}
	button := map[string]string{"right_click": "3", "middle_click": "2"}[r.action]
	if button == "" {
		button = "1"
	}
	return &result{}, c.withMods(r.mods, func() error { return c.xdo(append(args, button)...) })
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func (c *computer) move(p point) error {
	return c.xdo("mousemove", "--sync", strconv.Itoa(p.x), strconv.Itoa(p.y))
}

// withMods holds the modifiers down around f and always releases them.
func (c *computer) withMods(mods []string, f func() error) (err error) {
	for i, m := range mods {
		if err := c.xdo("keydown", m); err != nil {
			c.release(mods[:i])
			return err
		}
	}
	err = f()
	if rerr := c.release(mods); err == nil {
		err = rerr
	}
	return err
}

func (c *computer) release(mods []string) error {
	var first error
	for i := len(mods) - 1; i >= 0; i-- {
		if err := c.xdo("keyup", mods[i]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// typeText types in chunks of 50 characters, as Anthropic's reference
// implementation does: xdotool drops characters on long strings.
//
// A character the keyboard has no key for is bound to a spare keycode before
// the chunk and unbound after it. xdotool would bind it itself, but unbinds it
// right after the key event, often before the application has read it: the
// character is lost (seen with en dashes, « and ñ in Chrome).
func (c *computer) typeText(s screen, text string) error {
	for text != "" {
		n, i := 0, 0
		var extra []rune
		for i < len(text) && n < 50 {
			r, size := utf8.DecodeRuneInString(text[i:])
			if r > '~' && !slices.Contains(extra, r) {
				extra = append(extra, r)
			}
			i += size
			n++
		}
		n, err := c.typeChunk(s, extra, text[:i])
		if err != nil {
			return err
		}
		text = text[n:]
	}
	return nil
}

// typeChunk types chunk, or the start of it whose characters all have a key,
// and returns how many bytes it typed.
func (c *computer) typeChunk(s screen, extra []rune, chunk string) (int, error) {
	if len(extra) > 0 {
		n, unbind, err := s.Bind(extra)
		if err != nil {
			return 0, err
		}
		if n > 0 && n < len(extra) {
			// extra is in order of appearance: stop before the first one
			// left without a key.
			chunk = chunk[:strings.IndexRune(chunk, extra[n])]
		}
		// Applications read the new keymap when X tells them it changed,
		// and handle the last keys after xdotool has sent them.
		c.sleep(keymapSettle)
		defer func() {
			c.sleep(keysSettle)
			unbind()
		}()
	}
	return len(chunk), c.xdo("type", "--delay", "12", "--", chunk)
}

const (
	keymapSettle = 100 * time.Millisecond
	keysSettle   = 500 * time.Millisecond
)

func (c *computer) screenshot(s screen, full bool) (*result, error) {
	w, h, err := s.Size()
	if err != nil {
		return nil, err
	}
	img, err := s.Capture(image.Rect(0, 0, w, h))
	if err != nil {
		return nil, err
	}
	if !full {
		img = scaleImage(img, maxImageW, maxImageH)
	}
	res, err := imageResult(img, w, h)
	if err != nil {
		return nil, err
	}
	if c.statePath == "" {
		c.state = &shotState{w, h, res.width, res.height}
	} else if err := saveState(c.statePath, shotState{w, h, res.width, res.height}); err != nil {
		return nil, fmt.Errorf("saving the screenshot's scale: %v", err)
	}
	return res, nil
}

func (c *computer) zoom(s screen, g [4]int) (*result, error) {
	m, err := c.mapping(s)
	if err != nil {
		return nil, err
	}
	if g[0] < 0 || g[1] < 0 || g[2] > m.imgW || g[3] > m.imgH {
		return nil, fmt.Errorf("the zoom region %d,%d-%d,%d is outside the screenshot (%dx%d)", g[0], g[1], g[2], g[3], m.imgW, m.imgH)
	}
	r := image.Rect(
		divRound(g[0]*m.screenW, m.imgW), divRound(g[1]*m.screenH, m.imgH),
		divRound(g[2]*m.screenW, m.imgW), divRound(g[3]*m.screenH, m.imgH))
	img, err := s.Capture(r)
	if err != nil {
		return nil, err
	}
	return imageResult(scaleImage(img, maxImageW, maxImageH), m.screenW, m.screenH)
}

func imageResult(img *image.RGBA, screenW, screenH int) (*result, error) {
	b, err := encodePNG(img)
	if err != nil {
		return nil, err
	}
	return &result{png: b, width: img.Bounds().Dx(), height: img.Bounds().Dy(), screenW: screenW, screenH: screenH}, nil
}

// mapping converts between screenshot and screen coordinates.
type mapping struct{ screenW, screenH, imgW, imgH int }

// mapping reads the live screen size and the last screenshot's scale.
func (c *computer) mapping(s screen) (mapping, error) {
	w, h, err := s.Size()
	if err != nil {
		return mapping{}, err
	}
	st := c.state
	if c.statePath != "" {
		if st, err = loadState(c.statePath); err != nil {
			return mapping{}, err
		}
	}
	if st == nil {
		return mapping{w, h, w, h}, nil
	}
	if st.ScreenWidth != w || st.ScreenHeight != h {
		return mapping{}, fmt.Errorf("the screen is now %dx%d, it was %dx%d at the last screenshot: take a new screenshot",
			w, h, st.ScreenWidth, st.ScreenHeight)
	}
	return mapping{w, h, st.ImageWidth, st.ImageHeight}, nil
}

func (m mapping) toScreen(p point) (point, error) {
	if p.x < 0 || p.y < 0 || p.x >= m.imgW || p.y >= m.imgH {
		return point{}, fmt.Errorf("%d,%d is outside the screenshot (%dx%d)", p.x, p.y, m.imgW, m.imgH)
	}
	return point{divRound(p.x*m.screenW, m.imgW), divRound(p.y*m.screenH, m.imgH)}, nil
}

func (m mapping) toImage(p point) point {
	return point{
		min(m.imgW-1, max(0, divRound(p.x*m.imgW, m.screenW))),
		min(m.imgH-1, max(0, divRound(p.y*m.imgH, m.screenH))),
	}
}

// turn waits until no other action on the desktop runs, from this process
// or another, and returns what ends this one's turn. Waiting needs no turn.
func (c *computer) turn(r *request) (func(), error) {
	if r.action == "wait" {
		return func() {}, nil
	}
	c.mu.Lock()
	if c.lockPath == "" {
		return c.mu.Unlock, nil
	}
	// Opening without creating works on a file of another user in /tmp.
	f, err := os.Open(c.lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		f, err = os.OpenFile(c.lockPath, os.O_RDONLY|os.O_CREATE, 0o644)
	}
	if err == nil {
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
		}
	}
	if err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("waiting for the other actions: %v", err)
	}
	return func() { f.Close(); c.mu.Unlock() }, nil
}
