package main

import (
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"math/bits"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

func init() {
	// xgb logs connection errors on its own; ours carry the message.
	xgb.Logger = log.New(io.Discard, "", 0)
}

// screen is the X display, behind an interface so tests need no X server.
type screen interface {
	Size() (w, h int, err error)
	Capture(r image.Rectangle) (*image.RGBA, error)
	Pointer() (x, y int, err error)
	// Bind gives keys to the characters the keyboard has none for, until
	// unbind. When spare keycodes run out, only the first n have one.
	Bind(chars []rune) (n int, unbind func(), err error)
	Close()
}

const desktopTimeout = 20 * time.Second

// openDisplay connects to $DISPLAY, starting the VM's desktop if X is not up.
func openDisplay() (screen, error) {
	if d, err := dialDisplay(); err == nil {
		return d, nil
	}
	if out, err := exec.Command("sudo", "-n", "systemctl", "start", "boxe-desktop").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("cannot start the desktop: %s", firstLine(out, err))
	}
	deadline := time.Now().Add(desktopTimeout)
	for {
		d, err := dialDisplay()
		if err == nil {
			return d, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the desktop did not start within %v: %v", desktopTimeout, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func firstLine(out []byte, err error) string {
	if s, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); s != "" {
		return s
	}
	return err.Error()
}

type xDisplay struct {
	c      *xgb.Conn
	root   xproto.Window
	format pixelFormat
}

func dialDisplay() (*xDisplay, error) {
	c, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	setup := xproto.Setup(c)
	if len(setup.Roots) == 0 {
		c.Close()
		return nil, errors.New("the X server has no screens")
	}
	s := setup.Roots[0]
	f, err := rootFormat(setup, &s)
	if err != nil {
		c.Close()
		return nil, err
	}
	return &xDisplay{c: c, root: s.Root, format: f}, nil
}

// pixelFormat is how the server lays out ZPixmap pixels of the root window.
type pixelFormat struct {
	bpp                 int // bits per pixel: 16, 24 or 32
	pad                 int // scanline pad in bits
	msbFirst            bool
	rMask, gMask, bMask uint32
}

func rootFormat(setup *xproto.SetupInfo, s *xproto.ScreenInfo) (pixelFormat, error) {
	f := pixelFormat{msbFirst: setup.ImageByteOrder != xproto.ImageOrderLSBFirst}
	for _, pf := range setup.PixmapFormats {
		if pf.Depth == s.RootDepth {
			f.bpp, f.pad = int(pf.BitsPerPixel), int(pf.ScanlinePad)
		}
	}
	for _, d := range s.AllowedDepths {
		for _, v := range d.Visuals {
			if v.VisualId == s.RootVisual {
				if v.Class != xproto.VisualClassTrueColor && v.Class != xproto.VisualClassDirectColor {
					return f, fmt.Errorf("the X screen is not true color (visual class %d)", v.Class)
				}
				f.rMask, f.gMask, f.bMask = v.RedMask, v.GreenMask, v.BlueMask
			}
		}
	}
	switch {
	case f.bpp != 16 && f.bpp != 24 && f.bpp != 32:
		return f, fmt.Errorf("unsupported X pixel format: depth %d, %d bits per pixel", s.RootDepth, f.bpp)
	case f.rMask == 0 || f.gMask == 0 || f.bMask == 0:
		return f, errors.New("the X screen's root visual was not found")
	case f.pad == 0:
		f.pad = f.bpp
	}
	return f, nil
}

func (f pixelFormat) stride(w int) int {
	return (w*f.bpp + f.pad - 1) / f.pad * f.pad / 8
}

func (d *xDisplay) Size() (int, int, error) {
	g, err := xproto.GetGeometry(d.c, xproto.Drawable(d.root)).Reply()
	if err != nil {
		return 0, 0, err
	}
	return int(g.Width), int(g.Height), nil
}

func (d *xDisplay) Pointer() (int, int, error) {
	p, err := xproto.QueryPointer(d.c, d.root).Reply()
	if err != nil {
		return 0, 0, err
	}
	return int(p.RootX), int(p.RootY), nil
}

// stripBytes bounds each GetImage reply: one request for a whole large screen
// can exceed what the server is willing to send at once.
const stripBytes = 4 << 20

func (d *xDisplay) Capture(r image.Rectangle) (*image.RGBA, error) {
	img := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	stride := d.format.stride(r.Dx())
	rows := max(1, stripBytes/stride)
	for y := 0; y < r.Dy(); y += rows {
		h := min(rows, r.Dy()-y)
		rep, err := xproto.GetImage(d.c, xproto.ImageFormatZPixmap, xproto.Drawable(d.root),
			int16(r.Min.X), int16(r.Min.Y+y), uint16(r.Dx()), uint16(h), 0xffffffff).Reply()
		if err != nil {
			return nil, fmt.Errorf("screenshot: %v", err)
		}
		if len(rep.Data) < h*stride {
			return nil, fmt.Errorf("screenshot: short image from the X server (%d bytes, want %d)", len(rep.Data), h*stride)
		}
		d.format.convert(img, y, rep.Data, r.Dx(), h, stride)
	}
	return img, nil
}

func (d *xDisplay) Close() { d.c.Close() }

func (d *xDisplay) Bind(chars []rune) (int, func(), error) {
	setup := xproto.Setup(d.c)
	first, count := setup.MinKeycode, byte(setup.MaxKeycode-setup.MinKeycode+1)
	km, err := xproto.GetKeyboardMapping(d.c, first, count).Reply()
	if err != nil {
		return 0, nil, err
	}
	// Keys without symbols can still be modifiers (204 and 205 are Alt
	// and Meta on Xvnc): a character bound there never arrives.
	mm, err := xproto.GetModifierMapping(d.c).Reply()
	if err != nil {
		return 0, nil, err
	}
	per := int(km.KeysymsPerKeycode)
	bound := map[xproto.Keysym]bool{}
	var spare []xproto.Keycode
	for i := range int(count) {
		empty := !slices.Contains(mm.Keycodes, xproto.Keycode(int(first)+i))
		for _, k := range km.Keysyms[i*per : (i+1)*per] {
			if k != 0 {
				bound[k], empty = true, false
			}
		}
		if empty {
			spare = append(spare, xproto.Keycode(int(first)+i))
		}
	}
	var used []xproto.Keycode
	unbind := func() {
		for _, kc := range used {
			xproto.ChangeKeyboardMapping(d.c, 1, kc, byte(per), make([]xproto.Keysym, per))
		}
		xproto.GetInputFocus(d.c).Reply() // waits until the server has done it
	}
	n := 0
	for _, r := range chars {
		k := keysym(r)
		if bound[k] {
			n++
			continue
		}
		if len(spare) == 0 {
			break
		}
		syms := make([]xproto.Keysym, per)
		syms[0] = k
		if per > 1 {
			syms[1] = k // the same with shift
		}
		if err := xproto.ChangeKeyboardMappingChecked(d.c, 1, spare[0], byte(per), syms).Check(); err != nil {
			unbind()
			return 0, nil, err
		}
		used, spare, bound[k] = append(used, spare[0]), spare[1:], true
		n++
	}
	return n, unbind, nil
}

// keysym is the X keysym of a character: Latin-1 ones are their own code,
// the others Unicode keysyms.
func keysym(r rune) xproto.Keysym {
	if r >= 0x20 && r <= 0x7e || r >= 0xa0 && r <= 0xff {
		return xproto.Keysym(r)
	}
	return xproto.Keysym(0x01000000 | r)
}

// convert writes h rows of server pixels into img starting at row y0.
func (f pixelFormat) convert(img *image.RGBA, y0 int, data []byte, w, h, stride int) {
	if f.bpp == 32 && !f.msbFirst && f.rMask == 0xff0000 && f.gMask == 0xff00 && f.bMask == 0xff {
		// BGRX, what Xvnc and almost every server use.
		for y := range h {
			src := data[y*stride : y*stride+w*4]
			dst := img.Pix[(y0+y)*img.Stride:]
			for x := 0; x < w*4; x += 4 {
				dst[x], dst[x+1], dst[x+2], dst[x+3] = src[x+2], src[x+1], src[x], 0xff
			}
		}
		return
	}
	n := f.bpp / 8
	for y := range h {
		row := data[y*stride:]
		dst := img.Pix[(y0+y)*img.Stride:]
		for x := range w {
			var v uint32
			for i := range n {
				b := uint32(row[x*n+i])
				if f.msbFirst {
					v = v<<8 | b
				} else {
					v |= b << (8 * i)
				}
			}
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = channel(v, f.rMask), channel(v, f.gMask), channel(v, f.bMask), 0xff
		}
	}
}

// channel extracts the masked bits of v as an 8-bit value.
func channel(v, mask uint32) uint8 {
	shift := bits.TrailingZeros32(mask)
	width := bits.OnesCount32(mask)
	c := (v & mask) >> shift
	if width >= 8 {
		return uint8(c >> (width - 8))
	}
	return uint8(c * 255 / (1<<width - 1))
}
