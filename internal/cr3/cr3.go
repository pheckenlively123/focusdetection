// Package cr3 converts Canon CR3 raw files to JPEG using sips
// (os/exec; sips decodes CR3 natively via ImageIO).
//
// CR3 is an ISOBMFF container (ftyp brand "crx ") holding three CRAW
// tracks plus a CTMD metadata track and no embedded JPEG preview, so
// the raw must be developed. sips applies the camera's demosaic, color
// pipeline and sharpening — the same image a human sees when culling,
// which is the correct ground truth for subject-blur judgment.
package cr3

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// developTimeout bounds a single sips invocation. Measured develops are
// ~2.5–3 s; the timeout is a safety net against a hung decode.
const developTimeout = 60 * time.Second

// Develop develops cr3Path to JPEG, capped at maxDim on the long edge
// (sips -Z, aspect preserved), and returns the JPEG bytes.
func Develop(ctx context.Context, cr3Path string, maxDim int) ([]byte, error) {
	base := filepath.Base(cr3Path)
	if maxDim <= 0 {
		return nil, fmt.Errorf("cr3: %s: invalid max dim %d", base, maxDim)
	}
	dir, err := os.MkdirTemp("", "cr3blur-*")
	if err != nil {
		return nil, fmt.Errorf("cr3: %s: %w", base, err)
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "out.jpg")

	ctx, cancel := context.WithTimeout(ctx, developTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sips", "-s", "format", "jpeg",
		"-Z", fmt.Sprint(maxDim), cr3Path, "--out", out)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("cr3: %s: sips timed out after %s", base, developTimeout)
		}
		// sips writes diagnostics (e.g. "file could not be opened")
		// to stderr; include them for triage.
		if d := strings.TrimSpace(stderr.String()); d != "" {
			return nil, fmt.Errorf("cr3: %s: sips: %v: %s", base, err, d)
		}
		return nil, fmt.Errorf("cr3: %s: sips: %v", base, err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("cr3: %s: read sips output: %w", base, err)
	}
	return b, nil
}
