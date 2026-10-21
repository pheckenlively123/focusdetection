// Package afbox extracts the camera's autofocus box from the Canon
// maker notes in a CR3 file and scales it to the pixel space of the
// JPEG actually sent to the VLM.
//
// Canon records the AF area in the MakerNote AFInfo record (tag 0x0026):
// one (width, height, x, y) tuple per AF point in a center-origin
// coordinate space sized AFImageWidth x AFImageHeight (6000x4000 on the
// EOS R6 Mark II, 5472x3648 on the EOS R6). +X is right and +Y is up —
// verified against real R6/R6m2 shots: interpreting +Y as down places
// recorded boxes outside the frame, which the camera cannot produce.
//
// The valid points occupy a contiguous prefix of the tuple array
// (1053 slots on both cameras); the rest are zero. Single-point AF
// yields one tuple, zone AF yields a 9x9 (81-tuple) block, so the
// bounding box of all non-zero tuples is the focus region the camera
// used.
package afbox

import (
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/evanoberholster/imagemeta/meta/exif"
)

// Box is an axis-aligned rectangle in the top-left-origin pixel
// coordinates (x right, y down) of the image the VLM sees.
type Box struct {
	X, Y, W, H int
}

// String renders the box as "x,y,w,h" for reports and sidecars.
func (b Box) String() string {
	return fmt.Sprintf("%d,%d,%d,%d", b.X, b.Y, b.W, b.H)
}

// PromptText returns the sentence appended to the VLM prompt describing
// the box. imgW/imgH are the dimensions of the image the model sees
// (the box is already in that pixel space).
func (b Box) PromptText(imgW, imgH int) string {
	return fmt.Sprintf(
		" The camera's autofocus box for this photo is the rectangle at x=%d, y=%d, width=%d, height=%d pixels in this image (origin at top-left, x right, y down): a %s box in the %s. The camera focused on the subject inside this box. Judge focus only for the subject inside (or nearest to) this box; ignore other subjects outside it.",
		b.X, b.Y, b.W, b.H, sizeName(b, imgW, imgH), regionName(b, imgW, imgH))
}

// Info is the decoded AF record of one photo, in the camera's AF
// coordinate space.
type Info struct {
	// SpaceW/SpaceH is the AF coordinate space (AFImageWidth/Height,
	// falling back to CanonImageWidth/Height).
	SpaceW, SpaceH int
	// areas are the non-zero (w, h, centerX, centerY) tuples; +Y is up.
	areas []tuple
}

type tuple struct{ w, h, cx, cy int16 }

// Parse reads the Canon AF record from cr3Path. ok-style absence is
// reported via Info: a zero-area Info (no non-zero tuples) means there
// is no usable AF box (manual focus, no AF data, or not a Canon file).
func Parse(cr3Path string) (Info, error) {
	f, err := os.Open(cr3Path)
	if err != nil {
		return Info{}, fmt.Errorf("afbox: %w", err)
	}
	defer f.Close()

	// Only the Canon AF record is wanted; decode just its coordinates.
	x, err := exif.ParseWithReaderOptions(f,
		exif.WithAFInfoDecodeOptions(exif.AFInfoDecodeCoords))
	if err != nil {
		return Info{}, fmt.Errorf("afbox: %s: %w", filepath.Base(cr3Path), err)
	}
	c := x.MakerNote.Canon
	if c == nil {
		return Info{}, nil
	}
	af := c.AFInfo
	info := Info{
		SpaceW: int(af.AFImageWidth),
		SpaceH: int(af.AFImageHeight),
	}
	if info.SpaceW <= 0 || info.SpaceH <= 0 {
		info.SpaceW = int(af.CanonImageWidth)
		info.SpaceH = int(af.CanonImageHeight)
	}
	for _, p := range af.AFArea {
		if p[0] == 0 && p[1] == 0 && p[2] == 0 && p[3] == 0 {
			continue
		}
		info.areas = append(info.areas, tuple{p[0], p[1], p[2], p[3]})
	}
	return info, nil
}

// BoxFor scales the AF box into the pixel space of an imgW x imgH
// image — the resized JPEG the VLM sees. ok is false when there is no
// usable AF box: no data, manual focus, or a box that covers (nearly)
// the whole frame and would therefore constrain nothing.
func (in Info) BoxFor(imgW, imgH int) (Box, bool) {
	if in.SpaceW <= 0 || in.SpaceH <= 0 || imgW <= 0 || imgH <= 0 || len(in.areas) == 0 {
		return Box{}, false
	}
	l, t, r, b := 0, 0, 0, 0
	for i, p := range in.areas {
		x := in.SpaceW/2 + int(p.cx) - int(p.w)/2
		y := in.SpaceH/2 - int(p.cy) - int(p.h)/2 // +Y is up in the record
		if i == 0 || x < l {
			l = x
		}
		if i == 0 || y < t {
			t = y
		}
		if x+int(p.w) > r {
			r = x + int(p.w)
		}
		if y+int(p.h) > b {
			b = y + int(p.h)
		}
	}
	// Whole-frame AF (e.g. "Whole Area AF") gives no useful constraint.
	if r-l >= in.SpaceW-2 && b-t >= in.SpaceH-2 {
		return Box{}, false
	}

	sx := float64(imgW) / float64(in.SpaceW)
	sy := float64(imgH) / float64(in.SpaceH)
	box := Box{
		X: iround(float64(l) * sx),
		Y: iround(float64(t) * sy),
		W: iround(float64(r-l) * sx),
		H: iround(float64(b-t) * sy),
	}
	// Clamp into the frame (Canon allows boxes to spill slightly past
	// the edges) and drop degenerate results.
	if box.X < 0 {
		box.W += box.X
		box.X = 0
	}
	if box.Y < 0 {
		box.H += box.Y
		box.Y = 0
	}
	if box.X+box.W > imgW {
		box.W = imgW - box.X
	}
	if box.Y+box.H > imgH {
		box.H = imgH - box.Y
	}
	if box.W < 1 || box.H < 1 {
		return Box{}, false
	}
	return box, true
}

// regionName maps the box center onto a 3x3 grid for the prompt.
func regionName(b Box, imgW, imgH int) string {
	rows := [3]string{"upper", "middle", "lower"}
	cols := [3]string{"left", "center", "right"}
	c := 0
	if b.X+b.W/2 > 2*imgW/3 {
		c = 2
	} else if b.X+b.W/2 > imgW/3 {
		c = 1
	}
	r := 0
	if b.Y+b.H/2 > 2*imgH/3 {
		r = 2
	} else if b.Y+b.H/2 > imgH/3 {
		r = 1
	}
	if r == 1 && c == 1 {
		return "center"
	}
	return rows[r] + "-" + cols[c]
}

// sizeName buckets the box area relative to the frame for the prompt.
func sizeName(b Box, imgW, imgH int) string {
	frac := float64(b.W*b.H) / float64(imgW*imgH)
	switch {
	case frac < 0.02:
		return "small"
	case frac < 0.25:
		return "medium"
	default:
		return "large"
	}
}

func iround(v float64) int { return int(math.Round(v)) }
