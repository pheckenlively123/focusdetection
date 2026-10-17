package cr3

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeJPEG writes a small synthetic 100x40 JPEG (sips handles any
// decodable input; this exercises the Develop plumbing without a
// real CR3).
func makeJPEG(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "in.jpg")
	img := image.NewRGBA(image.Rect(0, 0, 100, 40))
	for x := 0; x < 100; x++ {
		for y := 0; y < 40; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 2), uint8(y * 6), 128, 255})
		}
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, nil); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	return p
}

func TestDevelop(t *testing.T) {
	in := makeJPEG(t, t.TempDir())
	b, err := Develop(context.Background(), in, 50)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("output is not a JPEG: %v", err)
	}
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != 50 || h != 20 {
		t.Errorf("resized to %dx%d, want 50x20 (long edge capped, aspect preserved)", w, h)
	}
}

func TestDevelopMissingFile(t *testing.T) {
	_, err := Develop(context.Background(), filepath.Join(t.TempDir(), "nope.CR3"), 1024)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "nope.CR3") {
		t.Errorf("error should name the file: %v", err)
	}
}

func TestDevelopInvalidMaxDim(t *testing.T) {
	if _, err := Develop(context.Background(), "x.CR3", 0); err == nil {
		t.Fatal("expected error for maxDim 0")
	}
}
