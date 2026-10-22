package afbox

import (
	"strings"
	"testing"
)

// r6m2SinglePoint reproduces the AF record of 5A8A7449.CR3 (EOS R6
// Mark II): one 328x328 box centered at (-208, -252) in the 6000x4000
// AF space. In +Y-up terms the center is left of and below the frame
// center.
func r6m2Info() Info {
	return Info{
		SpaceW: 6000,
		SpaceH: 4000,
		areas:  []tuple{{328, 328, -208, -252}},
	}
}

func TestBoxForSinglePoint(t *testing.T) {
	b, ok := r6m2Info().BoxFor(1024, 683)
	if !ok {
		t.Fatal("BoxFor: ok=false, want a box")
	}
	want := Box{X: 449, Y: 357, W: 56, H: 56}
	if b != want {
		t.Errorf("BoxFor = %v, want %v", b, want)
	}
}

// TestYAxisPointsUp pins the coordinate convention: +Y in the AF record
// is up. A tuple at cy=+252 (above center) must land above (smaller y)
// the tuple at cy=-252 (below center). Flipping this silently sends the
// prompt a box mirrored across the frame.
func TestYAxisPointsUp(t *testing.T) {
	above := Info{SpaceW: 6000, SpaceH: 4000, areas: []tuple{{328, 328, 0, 252}}}
	below := Info{SpaceW: 6000, SpaceH: 4000, areas: []tuple{{328, 328, 0, -252}}}
	ab, okA := above.BoxFor(1024, 683)
	be, okB := below.BoxFor(1024, 683)
	if !okA || !okB {
		t.Fatal("BoxFor: ok=false")
	}
	if ab.Y >= be.Y {
		t.Errorf("expected cy=+252 box above cy=-252 box: got %v vs %v", ab, be)
	}
	// Explicit values: cy=+252 -> top = 2000-252-164 = 1584 -> y=270.
	if ab.Y != 270 {
		t.Errorf("above-box y = %d, want 270", ab.Y)
	}
}

// TestBoxForZoneMerging checks that multiple tuples (zone AF) collapse
// to their bounding box.
func TestBoxForZoneMerging(t *testing.T) {
	in := Info{
		SpaceW: 6000,
		SpaceH: 4000,
		areas:  []tuple{{100, 100, -500, -200}, {100, 100, 500, 200}},
	}
	b, ok := in.BoxFor(1024, 683)
	if !ok {
		t.Fatal("BoxFor: ok=false, want a box")
	}
	want := Box{X: 418, Y: 299, W: 188, H: 85}
	if b != want {
		t.Errorf("BoxFor = %v, want %v", b, want)
	}
}

func TestBoxForWholeFrameRejected(t *testing.T) {
	// "Whole Area AF": the tuple covers the frame -> no constraint.
	in := Info{SpaceW: 6000, SpaceH: 4000, areas: []tuple{{6000, 4000, 0, 0}}}
	if _, ok := in.BoxFor(1024, 683); ok {
		t.Error("BoxFor: ok=true for whole-frame box, want false")
	}
}

func TestBoxForNoAreas(t *testing.T) {
	if _, ok := (Info{SpaceW: 6000, SpaceH: 4000}).BoxFor(1024, 683); ok {
		t.Error("BoxFor: ok=true with no areas, want false")
	}
	if _, ok := (Info{}).BoxFor(1024, 683); ok {
		t.Error("BoxFor: ok=true with zero space, want false")
	}
}

func TestBoxForClampsToFrame(t *testing.T) {
	// Box centered at cx=+2800 with width 1000 spills 300 AF units past
	// the right edge; it must be clamped into the 1024px image.
	in := Info{
		SpaceW: 6000,
		SpaceH: 4000,
		areas:  []tuple{{1000, 1000, 2800, 0}},
	}
	b, ok := in.BoxFor(1024, 683)
	if !ok {
		t.Fatal("BoxFor: ok=false, want a box")
	}
	if b.X < 0 || b.Y < 0 || b.X+b.W > 1024 || b.Y+b.H > 683 {
		t.Errorf("box %v not clamped into 1024x683", b)
	}
	want := Box{X: 905, Y: 256, W: 119, H: 171}
	if b != want {
		t.Errorf("BoxFor = %v, want %v", b, want)
	}
}

func TestPromptText(t *testing.T) {
	b := Box{X: 449, Y: 357, W: 56, H: 56}
	s := b.PromptText(1024, 683)
	for _, want := range []string{"x=449", "y=357", "width=56", "height=56", "small box", "center"} {
		if !strings.Contains(s, want) {
			t.Errorf("PromptText missing %q in %q", want, s)
		}
	}
}

func TestRegionName(t *testing.T) {
	cases := []struct {
		name string
		b    Box
		want string
	}{
		{"upper-left", Box{X: 10, Y: 10, W: 40, H: 40}, "upper-left"},
		{"upper-right", Box{X: 950, Y: 10, W: 40, H: 40}, "upper-right"},
		{"lower-left", Box{X: 10, Y: 600, W: 40, H: 40}, "lower-left"},
		{"lower-right", Box{X: 950, Y: 600, W: 40, H: 40}, "lower-right"},
		{"center", Box{X: 480, Y: 320, W: 60, H: 40}, "center"},
		{"middle-left", Box{X: 10, Y: 320, W: 40, H: 40}, "middle-left"},
		{"middle-right", Box{X: 950, Y: 320, W: 40, H: 40}, "middle-right"},
	}
	for _, c := range cases {
		if got := regionName(c.b, 1024, 683); got != c.want {
			t.Errorf("regionName(%v) = %q, want %q", c.b, got, c.want)
		}
	}
}
