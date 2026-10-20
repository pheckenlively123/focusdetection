// Command cr3blur walks a directory of Canon CR3 raw files, asks a
// vision LLM (served through llama-swap) whether the main subject of
// each photo is in focus, and writes an XMP sidecar next to each CR3
// recording the verdict.
//
// Exit codes: 0 = all files ok, 1 = fatal usage/env error,
// 2 = completed with per-file failures.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	cr3 "github.com/pheckenlively123/focusdetection/internal/cr3"
	"github.com/pheckenlively123/focusdetection/internal/vllm"
	"github.com/pheckenlively123/focusdetection/internal/xmp"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("cr3blur: ")

	url := flag.String("url", "http://127.0.0.1:8080",
		"OpenAI-compatible endpoint: default is the llama-swap listener (warmup triggers the lazy model load/swap); pass the direct llama-server port (e.g. http://127.0.0.1:9006, per the llama-swap config) to skip the proxy when the model is already resident")
	workers := flag.Int("workers", 4, "number of parallel workers")
	size := flag.Int("size", 1024, "max long edge (px) of the JPEG sent to the VLM")
	model := flag.String("model", "blur-detect", "llama-swap model alias")
	prompt := flag.String("prompt", "", "text prompt sent with each image (default: the built-in strict-JSON blur prompt; a custom prompt must still ask for the same JSON verdict)")
	force := flag.Bool("force", false, "re-analyze even if a sidecar with our namespace exists")
	reportPath := flag.String("report", "", "write the JSON report to this file (default: stdout)")
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatalf("usage: cr3blur [flags] DIR")
	}
	dir := flag.Arg(0)
	cr3s, err := findCR3(dir)
	if err != nil {
		log.Fatal(err)
	}
	if len(cr3s) == 0 {
		log.Fatalf("no .CR3 files found in %s", dir)
	}
	if strings.TrimSpace(*prompt) == "" {
		*prompt = vllm.DefaultPrompt
	} else {
		log.Printf("using custom prompt (%d chars)", len(*prompt))
	}
	log.Printf("found %d CR3 file(s) in %s", len(cr3s), dir)

	ctx := context.Background()
	// Warmup: verify the endpoint and (with the default llama-swap
	// listener URL) load/swap in the model before the pool starts, so
	// model load stays out of the per-file timing. Pointed directly at
	// the llama-server port, the model must already be loaded —
	// llama-swap loads it lazily and the port refuses connections
	// until then.
	log.Printf("warming up model %q at %s ...", *model, *url)
	warmStart := time.Now()
	if err := vllm.Warmup(ctx, *url, *model); err != nil {
		log.Fatal(err)
	}
	if d := time.Since(warmStart).Round(time.Millisecond); d > 10*time.Second {
		log.Printf("model ready (warmup %s — included a model load/swap; the llama-swap slot is single-tenant, so any other resident model was evicted)", d)
	} else {
		log.Printf("model ready (warmup %s)", d)
	}

	rep, err := run(ctx, dir, *url, *workers, *size, *model, *prompt, *force, cr3s)
	if err != nil {
		log.Fatal(err)
	}
	if err := writeReport(*reportPath, rep); err != nil {
		log.Fatal(err)
	}
	if rep.Failed > 0 {
		os.Exit(2)
	}
}

// findCR3 returns the .CR3 files under dir (recursively), sorted by name.
func findCR3(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".cr3") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// fileResult is the per-file entry in the report.
type fileResult struct {
	File          string `json:"file"`
	Status        string `json:"status"`           // sharp|blurry|failed|skipped
	Rating        int    `json:"rating,omitempty"` // star rating: 1 blurry, 2 in focus
	SubjectBlurry *bool  `json:"subject_blurry,omitempty"`
	Confidence    int    `json:"confidence,omitempty"`
	BlurType      string `json:"blur_type,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Error         string `json:"error,omitempty"`
}

// report is the JSON summary printed at the end of a run.
type report struct {
	Dir      string       `json:"dir"`
	Model    string       `json:"model"`
	Size     int          `json:"size"`
	Total    int          `json:"total"`    // CR3 files found
	Analyzed int          `json:"analyzed"` // processed in this run
	Skipped  int          `json:"skipped"`  // already had our sidecar
	Sharp    int          `json:"sharp"`
	Blurry   int          `json:"blurry"`
	Failed   int          `json:"failed"`
	Duration string       `json:"duration"`
	Files    []fileResult `json:"files"`
}

// run processes files with a fixed worker pool. The VLM is serialized
// by llama-server anyway; the pool parallelizes the sips develop step.
func run(ctx context.Context, dir, url string, workers, size int, model, prompt string, force bool, files []string) (report, error) {
	start := time.Now()
	rep := report{
		Dir:   dir,
		Model: model,
		Size:  size,
		Total: len(files),
		Files: make([]fileResult, len(files)),
	}
	for i, f := range files {
		rep.Files[i] = fileResult{File: filepath.Base(f)}
	}

	// Resume: skip files whose sidecar already carries our namespace.
	var jobIdx []int
	for i, f := range files {
		if !force {
			has, err := xmp.HasVerdict(f)
			if err != nil {
				return rep, err
			}
			if has {
				rep.Files[i].Status = "skipped"
				rep.Skipped++
				continue
			}
		}
		jobIdx = append(jobIdx, i)
	}
	if rep.Skipped > 0 {
		log.Printf("skipping %d file(s) with existing sidecars (-force to re-analyze)", rep.Skipped)
	}
	if len(jobIdx) == 0 {
		log.Println("nothing to do; all files already analyzed")
		rep.Duration = time.Since(start).Round(time.Millisecond).String()
		return rep, nil
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var done int
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				v, err := processFile(ctx, url, size, model, prompt, files[i])
				res := &rep.Files[i]
				if err == nil {
					res.Status = "sharp"
					if v.SubjectBlurry {
						res.Status = "blurry"
					}
					b := v.SubjectBlurry
					res.SubjectBlurry = &b
					res.Rating = v.Rating()
					res.Confidence = v.Confidence
					res.BlurType = v.BlurType
					res.Reason = v.Reason
				} else {
					res.Status = "failed"
					res.Error = err.Error()
				}
				mu.Lock()
				done++
				switch res.Status {
				case "sharp":
					rep.Sharp++
				case "blurry":
					rep.Blurry++
				case "failed":
					rep.Failed++
				}
				n := done
				mu.Unlock()
				log.Print(progressLine(n, rep.Total, res.File, res))
			}
		}()
	}
	for _, i := range jobIdx {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	rep.Analyzed = len(jobIdx)
	rep.Duration = time.Since(start).Round(time.Millisecond).String()
	return rep, nil
}

// progressLine renders the per-file progress line, e.g.
//
//	12/184  O47A0217.CR3  ★★ SHARP (conf 92)
//	13/184  O47A0218.CR3  ★ BLURRY (defocus, conf 71)
//	14/184  O47A0219.CR3  FAILED: sips: exit status 1
//
// The leading star(s) are the rating written to the sidecar: 2 for
// an in-focus subject, 1 for a blurry one.
func progressLine(n, total int, base string, res *fileResult) string {
	stars := strings.Repeat("★", res.Rating)
	switch res.Status {
	case "sharp":
		return fmt.Sprintf("%d/%d  %s  %s SHARP (conf %d)", n, total, base, stars, res.Confidence)
	case "blurry":
		return fmt.Sprintf("%d/%d  %s  %s BLURRY (%s, conf %d)", n, total, base, stars, res.BlurType, res.Confidence)
	default:
		return fmt.Sprintf("%d/%d  %s  FAILED: %s", n, total, base, res.Error)
	}
}

// processFile develops cr3Path to a resized JPEG, asks the VLM for a
// blur verdict, and writes an XMP sidecar recording the result.
func processFile(ctx context.Context, url string, size int, model, prompt, cr3Path string) (vllm.Verdict, error) {
	jpeg, err := cr3.Develop(ctx, cr3Path, size)
	if err != nil {
		return vllm.Verdict{}, err
	}
	v, err := vllm.Ask(ctx, url, model, prompt, jpeg)
	if err != nil {
		return vllm.Verdict{}, err
	}
	if err := xmp.WriteSidecar(ctx, cr3Path, v.SubjectBlurry, v.Rating(), v.Confidence, v.BlurType, v.Reason, model); err != nil {
		return v, err
	}
	return v, nil
}

// writeReport writes the JSON summary to path, or stdout when path
// is empty.
func writeReport(path string, rep report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if path == "" {
		_, err = os.Stdout.Write(b)
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
