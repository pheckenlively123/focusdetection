# focusdetection

Detect whether the **main subject** of a Canon CR3 raw photo is in focus —
defocus or motion blur — using a vision LLM, and record the verdict as an XMP
sidecar next to each CR3. Each photo also gets a star rating for culling:
**1 star** for a blurry subject, **2 stars** for an in-focus subject, written
to the sidecar as the standard `xmp:Rating` so culling tools (e.g.
Lightroom) display it.

Classic sharpness metrics (Laplacian variance, Tenengrad) measure the whole
frame and are fooled by "sharp background, blurry subject". A VLM understands
*what the subject is*, which is the right judge for subject-level blur.

```
.CR3 ──sips──▶ JPEG (≤1024 px) ──base64──▶ llama-swap :8080 ─▶ llama-server :9006
                                                                (Qwen2.5-VL-7B + mmproj)
report ◀── JSON verdicts ◀──────────────────────────────────────────────────────┘
sidecar ◀── .XMP (cr3blur: namespace) ◀─────────────────────────────────────────┘
```

By default `cr3blur` routes through the llama-swap listener
(`http://127.0.0.1:8080`), so its warmup triggers the lazy load/swap of
the `blur-detect` model — no separate load step. Pass
`-url http://127.0.0.1:9006` to talk to the llama-server port directly
instead (skips the proxy hop; the port refuses connections until
llama-swap has loaded the model, so it requires the model to already be
resident).

## Requirements

- **macOS** — CR3 development is done by `sips` (ImageIO), which decodes CR3
  natively. CR3 is an ISOBMFF container of raw tracks with **no embedded JPEG
  preview**, so the raw must be developed by something; `sips` applies the
  camera's demosaic/color/sharpening pipeline, i.e. exactly what a human sees
  when culling.
- **llama.cpp** (`llama-server` with vision support, `--mmproj`)
- **llama-swap** in front of it (OpenAI-compatible endpoint, model aliasing)
- **Qwen2.5-VL-7B-Instruct** (GGUF) + its mmproj file
- Go 1.26+ to build

## Setup

1. Download the model (Q8_0 ≈ 7.5 GiB + mmproj 1.3 GiB; the repo has no Q8
   mmproj, `mmproj-BF16.gguf` is the one):

   ```bash
   hf download unsloth/Qwen2.5-VL-7B-Instruct-GGUF \
     Qwen2.5-VL-7B-Instruct-Q8_0.gguf \
     mmproj-BF16.gguf \
     --local-dir ~/models/qwen2.5-vl-7b
   ```

2. Add a `blur-detect` entry to `~/.config/llama-swap/config.yaml` and restart
   llama-swap. `--mmproj` is a **llama-server** flag — llama-swap only passes
   the `cmd:` string through to the child process:

   ```yaml
     "blur-detect":
       cmd: >
         /opt/homebrew/bin/llama-server
         --port 9006
         -m /Users/<you>/models/qwen2.5-vl-7b/Qwen2.5-VL-7B-Instruct-Q8_0.gguf
         --mmproj /Users/<you>/models/qwen2.5-vl-7b/mmproj-BF16.gguf
         --ctx-size 8192
         -ngl 99
         --jinja
         --alias blur-detect
       proxy: http://127.0.0.1:9006
       checkEndpoint: /health
       ttl: 3600
   ```

3. Smoke-test one CR3 by hand (sips → curl → verdict); see
   [.planning/ai-blur-detect.md §5](.planning/ai-blur-detect.md).

## Build

```bash
make            # or: go build -o cr3blur ./cmd/cr3blur
```

Other targets: `make check` (fmt-check + vet + test), `make test`, `make vet`,
`make fmt`, `make tidy`, `make clean`.

One external Go dependency — [`evanoberholster/imagemeta`](https://github.com/evanoberholster/imagemeta)
(pure-Go EXIF/maker-note parsing, used for the autofocus box) — plus
`os/exec` for `sips`.

## Usage

```
cr3blur [flags] DIR
```

Walks `DIR` recursively for `.CR3` files, and for each one:
develops it to a resized JPEG, asks the VLM for a verdict, and writes an XMP
sidecar.

| Flag | Default | Description |
|---|---|---|
| `-url` | `http://127.0.0.1:8080` | OpenAI-compatible endpoint — default: the llama-swap listener (warmup triggers the lazy model load/swap); pass the direct llama-server port (e.g. `http://127.0.0.1:9006` from the llama-swap config) to skip the proxy when the model is already resident |
| `-workers` | `4` | parallel workers (sips parallelizes; the VLM is serialized by llama-server) |
| `-size` | `1024` | max long edge (px) of the JPEG sent to the VLM |
| `-model` | `blur-detect` | llama-swap model alias |
| `-prompt` | *(built-in)* | text prompt sent with each image (default: the built-in strict-JSON blur prompt; a custom prompt must still ask for the same JSON verdict) |
| `-force` | `false` | re-analyze even if a sidecar with our namespace exists |
| `-report` | *(stdout)* | write the JSON report to this file |

```bash
./cr3blur ~/PhotoOnDeck/2026-10-03-RoseHomecoming
./cr3blur -report run.json -size 1280 ~/PhotoOnDeck/2026-10-03-RoseHomecoming
./cr3blur -prompt 'Is the main subject tack-sharp? Ignore background softness. Reply with JSON only: {"subject_blurry":bool,"confidence":0-100,"blur_type":"none|defocus|motion|both","reason":"..."}' -force ~/PhotoOnDeck/2026-10-03-RoseHomecoming
```

Behavior:

- **Warmup**: before the worker pool starts, `cr3blur` does a health check plus
  a minimal completion against the alias, keeping model load/swap out of the
  per-file timing, and logs how long it took. With the default llama-swap
  listener URL this also triggers the lazy model load (a one-time ~30–90 s
  swap-in if another model is resident — the log line says so); pointed at
  the direct model port it only verifies the model is already up.
- **Resume**: files whose sidecar already carries our namespace are skipped
  (use `-force` to re-analyze) — matters for 500-shot folders and crash
  recovery.
- **Progress**: one line per completed file:

  ```
  12/184  O47A0217.CR3  ★★ SHARP (conf 92)  [AF 448,356,55,56]
  13/184  O47A0218.CR3  ★ BLURRY (defocus, conf 71)
  14/184  O47A0219.CR3  FAILED: sips: exit status 1
  ```

  The star(s) before the status are the rating written to the sidecar
  (2 = in focus, 1 = blurry). The trailing `[AF x,y,w,h]` is the camera's
  autofocus box in the sent JPEG's pixels (see [The prompt](#the-prompt)).

- **Report**: a JSON summary (per-file verdicts, star ratings, the per-file
  `af_box`, and sharp/blurry/failed counts) printed to stdout or written to
  `-report FILE`.
- **Exit codes**: `0` all ok, `1` fatal usage/env error, `2` completed with
  per-file failures.

### The prompt

Each image is sent with a strict-JSON prompt asking the model to check
defocus **and** motion blur and reply with a single JSON object:

```json
{"subject_blurry":bool,"confidence":0-100,"blur_type":"none|defocus|motion|both","reason":"..."}
```

**Autofocus box.** Canon records the AF box used for each shot in the CR3's
EXIF (MakerNote AFInfo record: one width/height/x/y tuple per AF point in a
center-origin space — 6000×4000 on the R6 Mark II, 5472×3648 on the R6,
+X right, +Y up). `cr3blur` takes the bounding box of the recorded AF points
(scales it to the resized JPEG the VLM actually sees, clamps to the frame)
and appends a sentence to the prompt: the box's pixel rectangle, a coarse
region label (e.g. "a small box in the center"), and the instruction to judge
focus **only** for the subject inside (or nearest to) the box and ignore
subjects outside it. That way the model judges what the camera actually
focused on, not some other subject that happens to be sharpest in frame.

- The box appears in the progress line and per-file report as
  `af_box: "x,y,w,h"` (pixels of the sent JPEG).
- No usable box → plain prompt, unchanged: manual focus, whole-frame AF
  ("Whole Area AF"), missing/undecodable maker notes, or a non-Canon file.
- Zone AF (81-point zones) yields the zone's bounding box; single-point AF
  yields the point's box (which on the R6/R6 II varies in size with the
  selected point — 243–647 AF units).

The built-in prompt can be overridden with `-prompt TEXT` — handy for
prompt calibration (e.g. teaching the model to ignore background softness
or shallow-DoF bokeh). The AF-box sentence is appended to custom prompts
too. The response is still parsed into the fixed JSON schema above, so a
custom prompt must ask the model to reply with the same object, or every
file fails verdict parsing.

The response is parsed defensively (markdown fences / stray prose tolerated),
validated against the `blur_type` enum and the 0–100 confidence range, and
retried (1 s / 5 s backoff) on network errors and HTTP 5xx only.

## XMP sidecars

One sidecar per CR3, using the Lightroom RAW sidecar convention:
`O47A0217.CR3` → `O47A0217.XMP` (same directory, same base name).

Verdicts live in a custom namespace — prefix `cr3blur:`,
URI `http://github.com/pheckenlively123/focusdetection/xmp/1.0/` — with
properties `SubjectBlurry`, `Rating`, `Confidence`, `BlurType`, `Reason`,
`SourceFile`, `Model`, and `AnalyzedAt`.

The star rating — **1** for a blurry subject, **2** for an in-focus subject
— is also written to the standard `xmp:Rating` property (namespace
`http://ns.adobe.com/xap/1.0/`) so culling tools like Lightroom display the
stars directly.

Existing sidecars are **never clobbered**: a sidecar carrying other
namespaces (e.g. Lightroom pick flags, captions) gets our
`<rdf:Description>` spliced in at the string level, everything else
preserved byte-for-byte. The one exception is the star rating itself: the
tool assigns it, so a pre-existing `<xmp:Rating>` (e.g. one Lightroom wrote)
is replaced with ours, leaving a single rating in the file. Writes are
atomic (temp file + rename).

## Repo layout

```
cmd/cr3blur/     CLI: flags, dir walk, worker pool, progress, JSON report
internal/cr3/    CR3 → JPEG via sips (os/exec; 60 s timeout, stderr capture)
internal/afbox/  Canon AF box from CR3 EXIF (maker notes) → prompt context,
                 scaled to the sent JPEG's pixels
internal/vllm/   llama-swap client: base64 image, strict-JSON prompt, retry,
                 verdict parsing, warmup
internal/xmp/    XMP sidecar read/write (no-clobber splice, atomic write)
internal/sharp/  optional Laplacian-variance triage (planned, not yet wired)
.planning/       design notes: ai-blur-detect.md (background), phase-02.md
```

## Tests

```bash
make check      # fmt-check + vet + test
```

(or `go build ./... && go vet ./... && go test ./...`)

Covered: XMP packet shape / well-formedness / splice-merge / no-clobber,
verdict parsing (fenced, prose-wrapped, invalid), the sips plumbing, and the
AF-box math (coordinate convention — +Y up — zone bounding boxes, scaling,
clamping, whole-frame rejection, prompt text).

## Caveats & operational notes

- VLMs are not calibrated meters: borderline shots may flip-flop and the
  confidence is self-reported. Use a threshold + spot-check the flagged set.
  (A first 184-file run came back 179 blurry / 5 sharp — the model reads
  "slightly soft" as blurry; prompt/threshold calibration is ongoing.)
- Downscale to ~1024 px before sending: big speedup, negligible effect on
  blur judgment.
- Throughput on an M2 Max: sips ~2.5–3 s/CR3 (parallel), VLM ~1–4 s/image
  (image prefill dominates) → roughly 20–30 photos/min when the model stays
  resident.
- **Don't run long `cr3blur` batches while other llama-swap clients are
  active.** The swap server is a single slot: concurrent use evicts
  `blur-detect` mid-run and a full swap cycle can exceed per-file timeouts.
  With a direct model-port URL (`-url http://127.0.0.1:9006`), an eviction
  surfaces as connection-refused on subsequent requests; through the
  llama-swap listener the next request waits out the swap instead.

## License

GPL v3 — see [LICENSE](LICENSE).
