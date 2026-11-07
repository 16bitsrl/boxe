package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
)

// Screenshots fit in 1280x800, a size every computer-use model takes and
// that keeps text readable.
const maxImageW, maxImageH = 1280, 800

// fitSize scales w x h down to fit maxW x maxH, keeping the aspect ratio.
// It never scales up.
func fitSize(w, h, maxW, maxH int) (int, int) {
	if w <= maxW && h <= maxH {
		return w, h
	}
	// Compare w/maxW with h/maxH without floating point.
	if w*maxH >= h*maxW {
		return maxW, max(1, divRound(h*maxW, w))
	}
	return max(1, divRound(w*maxH, h)), maxH
}

func divRound(a, b int) int { return (2*a + b) / (2 * b) }

// scaleImage returns img scaled to fit maxW x maxH (img itself if it fits).
func scaleImage(img *image.RGBA, maxW, maxH int) *image.RGBA {
	b := img.Bounds()
	w, h := fitSize(b.Dx(), b.Dy(), maxW, maxH)
	if w == b.Dx() && h == b.Dy() {
		return img
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// CatmullRom keeps small text sharp, which the model has to read; it
	// costs about 40 ms for 1920x1080 against 10 ms for ApproxBiLinear,
	// whose sparse sampling drops thin strokes when shrinking.
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// shotState is the screen and image size of the last full screenshot: the
// coordinate space of the actions that follow.
type shotState struct {
	ScreenWidth  int `json:"screen_width"`
	ScreenHeight int `json:"screen_height"`
	ImageWidth   int `json:"image_width"`
	ImageHeight  int `json:"image_height"`
}

// defaultStatePath is the same for every command of the user, whatever its
// environment (not TMPDIR, which agents may set): the screenshot of one
// command and the click of the next share one scale. MCP servers keep theirs.
func defaultStatePath() string {
	return fmt.Sprintf("/tmp/boxe-computer-%d.json", os.Getuid())
}

// loadState returns nil when no screenshot was taken yet.
func loadState(path string) (*shotState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s shotState
	if err := json.Unmarshal(b, &s); err != nil || s.ScreenWidth <= 0 || s.ScreenHeight <= 0 || s.ImageWidth <= 0 || s.ImageHeight <= 0 {
		return nil, fmt.Errorf("%s is damaged: remove it and take a new screenshot", path)
	}
	return &s, nil
}

func saveState(path string, s shotState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".boxe-computer-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600 already.
	return os.Rename(f.Name(), path)
}
