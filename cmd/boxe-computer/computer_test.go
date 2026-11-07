package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeScreen is a display of a given size with a gradient on it.
type fakeScreen struct {
	w, h     int
	px, py   int
	captures []image.Rectangle
	bound    []string // the characters of each Bind
	unbound  int
	spare    int // keycodes Bind has, 0 for enough
}

func (f *fakeScreen) Size() (int, int, error)    { return f.w, f.h, nil }
func (f *fakeScreen) Pointer() (int, int, error) { return f.px, f.py, nil }
func (f *fakeScreen) Close()                     {}
func (f *fakeScreen) Bind(chars []rune) (int, func(), error) {
	f.bound = append(f.bound, string(chars))
	n := len(chars)
	if f.spare > 0 {
		n = min(n, f.spare)
	}
	return n, func() { f.unbound++ }, nil
}
func (f *fakeScreen) Capture(r image.Rectangle) (*image.RGBA, error) {
	f.captures = append(f.captures, r)
	img := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := range r.Dy() {
		for x := range r.Dx() {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 0, 0xff})
		}
	}
	return img, nil
}

type fakeComputer struct {
	*computer
	scr   *fakeScreen
	calls [][]string
	slept []time.Duration
}

func newFake(t *testing.T, w, h int) *fakeComputer {
	f := &fakeComputer{scr: &fakeScreen{w: w, h: h}}
	f.computer = &computer{
		open:      func() (screen, error) { return f.scr, nil },
		xdo:       func(args ...string) error { f.calls = append(f.calls, args); return nil },
		sleep:     func(d time.Duration) { f.slept = append(f.slept, d) },
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	return f
}

func (f *fakeComputer) run(t *testing.T, args ...string) *result {
	t.Helper()
	r, err := parseArgs(args, nil)
	if err != nil {
		t.Fatalf("%q: %v", args, err)
	}
	res, err := f.do(r)
	if err != nil {
		t.Fatalf("%q: %v", args, err)
	}
	return res
}

func argv(lines ...string) [][]string {
	var out [][]string
	for _, l := range lines {
		out = append(out, strings.Fields(l))
	}
	return out
}

func TestXdotoolArgv(t *testing.T) {
	tests := []struct {
		args []string
		want [][]string
	}{
		{[]string{"left_click"}, argv("click 1")},
		{[]string{"left_click", "10", "20"}, argv("mousemove --sync 10 20", "click 1")},
		{[]string{"right_click", "10", "20"}, argv("mousemove --sync 10 20", "click 3")},
		{[]string{"middle_click"}, argv("click 2")},
		{[]string{"double_click"}, argv("click --repeat 2 --delay 80 1")},
		{[]string{"triple_click", "1", "2", "--mod", "ctrl+shift"},
			argv("mousemove --sync 1 2", "keydown ctrl", "keydown shift", "click --repeat 3 --delay 80 1", "keyup shift", "keyup ctrl")},
		{[]string{"left_click_drag", "1", "2", "3", "4"},
			argv("mousemove --sync 1 2", "mousedown 1", "mousemove --sync 3 4", "mouseup 1")},
		{[]string{"mouse_move", "5", "6"}, argv("mousemove --sync 5 6")},
		{[]string{"left_mouse_down"}, argv("mousedown 1")},
		{[]string{"left_mouse_up"}, argv("mouseup 1")},
		{[]string{"scroll", "up"}, argv("click --repeat 3 --delay 30 4")},
		{[]string{"scroll", "down", "5", "7", "8", "--mod", "alt"},
			argv("mousemove --sync 7 8", "keydown alt", "click --repeat 5 --delay 30 5", "keyup alt")},
		{[]string{"scroll", "left", "1"}, argv("click --repeat 1 --delay 30 6")},
		{[]string{"scroll", "right"}, argv("click --repeat 3 --delay 30 7")},
		{[]string{"key", "enter"}, argv("key --repeat 1 --delay 50 -- Return")},
		{[]string{"key", "ctrl+a", "Delete", "--repeat", "4"}, argv("key --repeat 4 --delay 50 -- ctrl+a Delete")},
		{[]string{"hold_key", "shift", "2"}, argv("keydown -- shift", "keyup -- shift")},
		{[]string{"type", "hi", "there"}, [][]string{{"type", "--delay", "12", "--", "hi there"}}},
		{[]string{"wait", "1"}, nil},
	}
	for _, tt := range tests {
		f := newFake(t, 1280, 800)
		f.run(t, tt.args...)
		if !reflect.DeepEqual(f.calls, tt.want) {
			t.Errorf("%q:\n got %q\nwant %q", tt.args, f.calls, tt.want)
		}
	}
}

func TestTypeChunks(t *testing.T) {
	f := newFake(t, 100, 100)
	text := strings.Repeat("è", 120) // 2 bytes each: chunks count runes
	f.run(t, "type", text)
	if len(f.calls) != 3 {
		t.Fatalf("got %d calls", len(f.calls))
	}
	for i, n := range []int{50, 50, 20} {
		if got := []rune(f.calls[i][4]); len(got) != n {
			t.Errorf("chunk %d: %d runes, want %d", i, len(got), n)
		}
	}
}

func TestWaitAndHold(t *testing.T) {
	f := newFake(t, 100, 100)
	f.run(t, "wait", "1.5")
	f.run(t, "hold_key", "a", "0.25")
	want := []time.Duration{1500 * time.Millisecond, 250 * time.Millisecond}
	if !reflect.DeepEqual(f.slept, want) {
		t.Errorf("slept %v, want %v", f.slept, want)
	}
}

func TestFitSize(t *testing.T) {
	tests := []struct{ w, h, ww, wh int }{
		{1280, 800, 1280, 800},
		{800, 600, 800, 600},
		{1600, 900, 1280, 720},
		{1920, 1080, 1280, 720},
		{1710, 1128, 1213, 800},
		{2560, 1600, 1280, 800},
		{1000, 2000, 400, 800},
		{3000, 10, 1280, 4},
		{10000, 1, 1280, 1},
	}
	for _, tt := range tests {
		if w, h := fitSize(tt.w, tt.h, 1280, 800); w != tt.ww || h != tt.wh {
			t.Errorf("%dx%d: got %dx%d, want %dx%d", tt.w, tt.h, w, h, tt.ww, tt.wh)
		}
	}
}

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestScreenshotScaling(t *testing.T) {
	f := newFake(t, 1600, 900)
	// Before any screenshot, coordinates are screen pixels.
	f.run(t, "mouse_move", "1500", "850")
	res := f.run(t, "screenshot")
	if res.width != 1280 || res.height != 720 || res.screenW != 1600 || res.screenH != 900 {
		t.Fatalf("got %+v", res)
	}
	if b := decodePNG(t, res.png).Bounds(); b.Dx() != 1280 || b.Dy() != 720 {
		t.Fatalf("png is %v", b)
	}
	f.calls = nil
	f.run(t, "left_click", "640", "360")
	f.run(t, "mouse_move", "1279", "719")
	f.run(t, "left_click_drag", "0", "0", "100", "1")
	want := argv("mousemove --sync 800 450", "click 1", "mousemove --sync 1599 899",
		"mousemove --sync 0 0", "mousedown 1", "mousemove --sync 125 1", "mouseup 1")
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("got %q\nwant %q", f.calls, want)
	}

	f.scr.px, f.scr.py = 800, 450
	if res := f.run(t, "cursor_position"); *res.cursor != (point{640, 360}) {
		t.Errorf("cursor at %v", *res.cursor)
	}

	// Outside the image.
	for _, args := range [][]string{{"left_click", "1280", "0"}, {"mouse_move", "0", "720"}, {"mouse_move", "-1", "0"}, {"zoom", "0", "0", "1281", "10"}} {
		r, _ := parseArgs(args, nil)
		if _, err := f.do(r); err == nil || !strings.Contains(err.Error(), "outside the screenshot") {
			t.Errorf("%q: got %v", args, err)
		}
	}

	// A full screenshot resets the scale to 1.
	if res := f.run(t, "screenshot", "--full"); res.width != 1600 || res.height != 900 {
		t.Errorf("full: %dx%d", res.width, res.height)
	}
	f.calls = nil
	f.run(t, "mouse_move", "1500", "850")
	if !reflect.DeepEqual(f.calls, argv("mousemove --sync 1500 850")) {
		t.Errorf("after --full: %q", f.calls)
	}
}

func TestScreenSizeChanged(t *testing.T) {
	f := newFake(t, 1600, 900)
	f.run(t, "screenshot")
	f.scr.w, f.scr.h = 1710, 1128
	f.calls = nil
	for _, args := range [][]string{{"left_click", "1", "1"}, {"mouse_move", "1", "1"}, {"cursor_position"}, {"zoom", "0", "0", "10", "10"}, {"scroll", "up", "1", "1"}} {
		r, _ := parseArgs(args, nil)
		_, err := f.do(r)
		want := "the screen is now 1710x1128, it was 1600x900 at the last screenshot: take a new screenshot"
		if err == nil || err.Error() != want {
			t.Errorf("%q: got %v", args, err)
		}
	}
	if f.calls != nil {
		t.Errorf("xdotool ran: %q", f.calls)
	}
	// Actions without coordinates go on, and a new screenshot fixes it.
	f.run(t, "key", "Return")
	f.run(t, "screenshot")
	f.run(t, "mouse_move", "1", "1")
}

func TestZoom(t *testing.T) {
	f := newFake(t, 1600, 900)
	f.run(t, "screenshot")
	before, _ := loadState(f.statePath)
	res := f.run(t, "zoom", "0", "0", "640", "360")
	// The region in screen pixels, at native resolution (800x450 fits).
	if got := f.scr.captures[len(f.scr.captures)-1]; got != image.Rect(0, 0, 800, 450) {
		t.Errorf("captured %v", got)
	}
	if res.width != 800 || res.height != 450 || res.screenW != 1600 {
		t.Errorf("got %+v", res)
	}
	if after, _ := loadState(f.statePath); *after != *before {
		t.Errorf("zoom changed the state: %+v", *after)
	}
	// A large region is scaled to fit.
	res = f.run(t, "zoom", "0", "0", "1280", "720")
	if res.width != 1280 || res.height != 720 {
		t.Errorf("full region: %dx%d", res.width, res.height)
	}
}

func TestCLIOutput(t *testing.T) {
	f := newFake(t, 1600, 900)
	var out, errb bytes.Buffer
	if code := run([]string{"--json", "screenshot"}, nil, &out, &errb, f.computer); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var shot struct {
		Width, Height int
		ScreenW       int `json:"screen_width"`
		ScreenH       int `json:"screen_height"`
		PNG           string
	}
	if err := json.Unmarshal(out.Bytes(), &shot); err != nil || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("%q: %v", out.String(), err)
	}
	b, _ := base64.StdEncoding.DecodeString(shot.PNG)
	if shot.Width != 1280 || shot.Height != 720 || shot.ScreenW != 1600 || shot.ScreenH != 900 || decodePNG(t, b).Bounds().Dx() != 1280 {
		t.Errorf("got %+v", shot)
	}

	check := func(args []string, code int, stdout, stderr string) {
		t.Helper()
		out.Reset()
		errb.Reset()
		if got := run(args, nil, &out, &errb, f.computer); got != code || out.String() != stdout || errb.String() != stderr {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, got, out.String(), errb.String())
		}
	}
	f.scr.px, f.scr.py = 800, 450
	check([]string{"cursor_position"}, 0, "640 360\n", "")
	check([]string{"--json", "cursor-position"}, 0, `{"x":640,"y":360}`+"\n", "")
	check([]string{"--json", "left-click", "1", "1"}, 0, `{"action":"left_click","status":"done"}`+"\n", "")
	check([]string{"left_click", "1", "1", "--json"}, 0, `{"action":"left_click","status":"done"}`+"\n", "")
	check([]string{"left_click"}, 0, "", "")
	check([]string{"left_click", "5000", "1"}, 1, "", "boxe-computer: 5000,1 is outside the screenshot (1280x720)\n")
	check([]string{"fly"}, 2, "", "boxe-computer: unknown action \"fly\" (boxe-computer help)\n")
	check([]string{"mcp", "x"}, 2, "", "boxe-computer: mcp takes no arguments\n")

	path := filepath.Join(t.TempDir(), "s.png")
	check([]string{"screenshot", "--out", path}, 0, "screen 1600x900, image 1280x720\n", "")
	out.Reset()
	if code := run([]string{"screenshot"}, nil, &out, &errb, f.computer); code != 0 || decodePNG(t, out.Bytes()).Bounds().Dx() != 1280 {
		t.Errorf("png to stdout: exit %d", code)
	}
}

func TestPixelFormats(t *testing.T) {
	px := [2]color.RGBA{{0x12, 0x34, 0x56, 0xff}, {0xff, 0x80, 0x00, 0xff}}
	tests := []struct {
		name string
		f    pixelFormat
		data []byte
	}{
		{"bgrx", pixelFormat{bpp: 32, pad: 32, rMask: 0xff0000, gMask: 0xff00, bMask: 0xff},
			[]byte{0x56, 0x34, 0x12, 0, 0x00, 0x80, 0xff, 0}},
		{"xrgb msb", pixelFormat{bpp: 32, pad: 32, msbFirst: true, rMask: 0xff0000, gMask: 0xff00, bMask: 0xff},
			[]byte{0, 0x12, 0x34, 0x56, 0, 0xff, 0x80, 0x00}},
		{"rgbx lsb", pixelFormat{bpp: 32, pad: 32, rMask: 0xff, gMask: 0xff00, bMask: 0xff0000},
			[]byte{0x12, 0x34, 0x56, 0, 0xff, 0x80, 0x00, 0}},
		{"24 bpp padded", pixelFormat{bpp: 24, pad: 32, rMask: 0xff0000, gMask: 0xff00, bMask: 0xff},
			[]byte{0x56, 0x34, 0x12, 0x00, 0x80, 0xff, 0, 0}},
	}
	for _, tt := range tests {
		img := image.NewRGBA(image.Rect(0, 0, 2, 1))
		tt.f.convert(img, 0, tt.data, 2, 1, tt.f.stride(2))
		for x := range 2 {
			if got := img.RGBAAt(x, 0); got != px[x] {
				t.Errorf("%s: pixel %d is %v, want %v", tt.name, x, got, px[x])
			}
		}
	}
	// 16 bits, 565: full red and full blue.
	f := pixelFormat{bpp: 16, pad: 32, rMask: 0xf800, gMask: 0x07e0, bMask: 0x001f}
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	f.convert(img, 0, []byte{0x00, 0xf8, 0x1f, 0x00}, 2, 1, f.stride(2))
	if img.RGBAAt(0, 0) != (color.RGBA{0xff, 0, 0, 0xff}) || img.RGBAAt(1, 0) != (color.RGBA{0, 0, 0xff, 0xff}) {
		t.Errorf("565: %v %v", img.RGBAAt(0, 0), img.RGBAAt(1, 0))
	}
}

func BenchmarkScreenshot1080p(b *testing.B) {
	img := (&fakeScreen{}).mustCapture(1920, 1080)
	for b.Loop() {
		if _, err := encodePNG(scaleImage(img, maxImageW, maxImageH)); err != nil {
			b.Fatal(err)
		}
	}
}

func (f *fakeScreen) mustCapture(w, h int) *image.RGBA {
	img, _ := f.Capture(image.Rect(0, 0, w, h))
	return img
}

func TestTypeBindsWhatTheKeyboardLacks(t *testing.T) {
	f := newFake(t, 100, 100)
	f.run(t, "type", "a–b«a–")
	if want := []string{"–«"}; !reflect.DeepEqual(f.scr.bound, want) {
		t.Errorf("bound %q, want %q", f.scr.bound, want)
	}
	if f.scr.unbound != 1 {
		t.Errorf("unbound %d times, want 1", f.scr.unbound)
	}
	f = newFake(t, 100, 100)
	f.run(t, "type", "plain ascii~")
	if f.scr.bound != nil {
		t.Errorf("bound %q for ASCII", f.scr.bound)
	}
}

func TestHoldKeyReleasesAfterAFailedKeydown(t *testing.T) {
	f := newFake(t, 100, 100)
	f.xdo = func(args ...string) error {
		f.calls = append(f.calls, args)
		if args[0] == "keydown" {
			return errors.New("No such key name")
		}
		return nil
	}
	r, _ := parseArgs([]string{"hold_key", "shift+Foo", "1"}, nil)
	if _, err := f.do(r); err == nil {
		t.Fatal("no error")
	}
	if want := argv("keydown -- shift+Foo", "keyup -- shift+Foo"); !reflect.DeepEqual(f.calls, want) {
		t.Errorf("got %q, want %q", f.calls, want)
	}
}

func TestMCPKeepsItsOwnScale(t *testing.T) {
	f := newFake(t, 1600, 900)
	shared := f.statePath
	f.statePath = ""
	f.run(t, "screenshot")
	if _, err := os.Stat(shared); err == nil {
		t.Error("the MCP screenshot wrote the shared scale")
	}
	f.run(t, "left_click", "1279", "719")
	if want := argv("mousemove --sync 1599 899", "click 1"); !reflect.DeepEqual(f.calls, want) {
		t.Errorf("got %q, want %q", f.calls, want)
	}
}

func TestTypeSplitsWhenKeysRunOut(t *testing.T) {
	f := newFake(t, 100, 100)
	f.scr.spare = 2
	f.run(t, "type", "aàbèc–dè")
	want := [][]string{
		{"type", "--delay", "12", "--", "aàbèc"},
		{"type", "--delay", "12", "--", "–dè"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("got %q, want %q", f.calls, want)
	}
	if wantB := []string{"àè–", "–è"}; !reflect.DeepEqual(f.scr.bound, wantB) {
		t.Errorf("bound %q, want %q", f.scr.bound, wantB)
	}
}

// The longest text accepted, in the slowest shapes, ends well within the
// five minutes boxed gives a command in the VM: typing at 12 ms a
// character plus the pauses around each chunk with keys to bind.
func TestLongestTextEndsInTime(t *testing.T) {
	fill := func(unit string) string {
		return strings.Repeat(unit, maxTypeText/len(unit))
	}
	distinct := []rune("àèéìòùÀÈÉÌÒÙçñßøåæœ€£")
	for name, text := range map[string]string{
		"one accent per chunk":   fill(strings.Repeat("a", 49) + "é"),
		"more accents than keys": fill(string(distinct)),
	} {
		f := newFake(t, 100, 100)
		f.scr.spare = 19
		f.run(t, "type", text)
		total := time.Duration(0)
		for _, d := range f.slept {
			total += d
		}
		for _, c := range f.calls {
			total += time.Duration(len([]rune(c[len(c)-1]))) * 12 * time.Millisecond
		}
		if total > 4*time.Minute {
			t.Errorf("%s: %d bytes take %s", name, len(text), total)
		}
	}
}

// Actions from two agents, or an agent and a command, never interleave:
// one click must not land where the other moved the mouse.
func TestActionsTakeTurns(t *testing.T) {
	f := newFake(t, 1280, 800)
	f.run(t, "screenshot")
	var mu sync.Mutex
	f.xdo = func(args ...string) error {
		mu.Lock()
		f.calls = append(f.calls, args)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		return nil
	}
	f.statePath = ""
	f.state = &shotState{1280, 800, 1280, 800}
	f.lockPath = filepath.Join(t.TempDir(), "lock")
	var wg sync.WaitGroup
	for _, xy := range []string{"10", "20"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := callTool(f.computer, json.RawMessage(`{"action":"left_click","coordinate":[`+xy+`,`+xy+`]}`)); res.IsError {
				t.Errorf("left_click: %+v", res.Content)
			}
		}()
	}
	wg.Wait()
	for i := 0; i+1 < len(f.calls); i += 2 {
		if f.calls[i][0] != "mousemove" || f.calls[i+1][0] != "click" {
			t.Fatalf("interleaved: %q", f.calls)
		}
	}
}
