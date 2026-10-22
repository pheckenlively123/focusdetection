# AI Blur Detection for Canon CR3 (Go + llama.cpp + llama-swap)

Project notes for detecting **blurry subject** in Canon CR3 raw files using a
vision LLM running in llama.cpp, served through llama-swap.

Status: design + environment validated. Model download, llama-swap config
entry, and the Go tool are the remaining build steps.

---

## 1. Goal

Given a folder of Canon CR3 files, flag the ones where the **main subject** is
blurry (defocus or motion blur). A VLM is the right judge here because it
understands *what the subject is* — classic sharpness metrics (Laplacian
variance, Tenengrad) are fooled by "sharp background, blurry subject".

Planned pipeline:

```
.CR3 ──sips──▶ JPEG (≤1024 px) ──base64──▶ llama-swap :8080 ─▶ llama-server :9006
                                                          (Qwen2.5-VL-7B + mmproj)
report ◀── JSON verdicts ◀───────────────────────────────────┘
```

Written in **Go** (single static binary, stdlib + `os/exec` for sips,
goroutine worker pool). No Python, no pip.

## 2. Target environment (verified)

| Item | Value |
|---|---|
| Machine | MacBook Pro 14" (Mac14,13), Apple M2 Max, 64 GB RAM |
| Metal working-set ceiling | ~53 GB (from llama-swap config comments) |
| Go | 1.26.3 darwin/arm64 |
| hf CLI | 2.2.0 (`/opt/homebrew/bin/hf`) |
| llama.cpp | 0.7.0 via Homebrew; `llama-server` has full vision support (`--mmproj`) |
| llama-swap | `~/go/bin/llama-swap`, config `~/.config/llama-swap/config.yaml`, listens `:8080` |
| Ports in use | 9001 gemma-4-e2b, 9002 gemma-4-31b, 9003 muse-glimmer-30b, 9004 qwen3.8-27b, 9005 qwen3-coder-next → **use 9006** |
| sips | `/usr/bin/sips` — decodes CR3 natively (ImageIO) |
| libraw | installed via Homebrew (only needed for optional raw-domain work) |
| ImageMagick | installed, **do not use for CR3** (its delegate execs missing `darktable-cli` and fails) |

Sample CR3 files for testing:
- `/Users/patrickheckenlively/PhotoOnDeck/2026-09-14-HillsdaleFlyIn/5A8A0324.CR3` (24 MP, 6000×4000)
- `/Users/patrickheckenlively/PhotoOnDeck/2026-10-03-RoseHomecoming/O47A0217.CR3` (24 MP)

## 3. CR3 format findings (verified by parsing real files)

- CR3 is an **ISOBMFF container** (MP4 family): `ftyp` box with brand
  `crx `, then `moov` + `mdat`.
- `moov` contains 4 tracks:
  - 3× `CRAW` — the full raw (~20.9 MB for 24 MP) plus **two reduced raw
    streams** (camera fast-view previews)
  - 1× `CTMD` — small metadata track
- **No embedded JPEG preview** — the reduced tracks are raw, not JPEG.
  There is no "carve out the preview JPEG" shortcut for CR3; the raw must be
  developed by something.
- `sips` develops CR3 correctly:
  - `sips -s format jpeg in.CR3 --out out.jpg` → 6000×4000, ~2.5 s
  - `sips -s format jpeg -Z 1024 in.CR3 --out out.jpg` → 1024×682, ~2.8 s
    (measured; `-Z` = max dimension, aspect preserved)
- Fidelity note: sips output is a *developed* image (demosaicked, camera
  color pipeline, camera sharpening). That matches what a human sees when
  culling — the correct ground truth for subject-blur judgment.
- **The autofocus box IS in the EXIF** (verified on EOS R6 and R6 Mark II
  CR3s, exiftool + imagemeta): Canon's MakerNote **AFInfo record** (tag
  0x0026) holds one width/height/x/y tuple per AF point (1053 slots on
  both bodies; valid points occupy a contiguous prefix — 1 tuple for
  single-point AF, 81 for a 9×9 zone) in a **center-origin** coordinate
  space sized by AFImageWidth/Height: **6000×4000 on the R6 Mark II,
  5472×3648 on the R6** (== the developed frame size in both cases).
  **Axes: +X right, +Y up** — proven, not assumed: under a Y-down reading,
  ~40% of a real shoot's recorded boxes fall entirely outside the frame
  (impossible); under Y-up every box is in-frame. Converting to top-left
  image pixels: `left = W/2 + x − w/2`, `top = H/2 − y − h/2`.
  Box sizes vary per AF point (243–647 AF units on the R6m2) — the
  "range of sizes" visible on the camera. The AFAreaMode word gives the
  mode (e.g. 7 = Zone AF, 19–21 = Flexible Zone AF 1/2/3, 22 = Whole
  Area). Older DSLRs (pre-2010s) use a different AFInfo layout (tag
  0x0012) — not needed for the R-series.
- sips **drops the Canon maker notes** in its JPEG output, so the box
  must be read from the CR3 itself (Go: `evanoberholster/imagemeta`
  decodes the CR3's ISOBMFF + Canon maker notes; v1.0.0 works on Go 1.24+).
  Note: imagemeta's own derived `AFPoints` rectangles assume +Y *down* —
  wrong for these cameras; compute from the raw `AFArea` tuples instead.

## 4. Vision model

Model: **Qwen2.5-VL-7B-Instruct** (GGUF), from `unsloth/Qwen2.5-VL-7B-Instruct-GGUF`.
Strong at structured output and at reasoning about what the subject is.
Q8_0 ≈ 7.5 GiB + mmproj 1.3 GiB — fits comfortably in the 53 GB Metal ceiling.

### Download (hf CLI, verified file names)

```bash
hf download unsloth/Qwen2.5-VL-7B-Instruct-GGUF \
  Qwen2.5-VL-7B-Instruct-Q8_0.gguf \
  mmproj-BF16.gguf \
  --local-dir ~/models/qwen2.5-vl-7b
```

Note: the repo has no Q8 mmproj; `mmproj-BF16.gguf` (1,291 MiB) is the one.

### llama-swap config entry

Add to `~/.config/llama-swap/config.yaml` (then restart llama-swap).
`--mmproj` is a **llama-server** flag; llama-swap only passes the `cmd:`
string through to the child process (llama-swap's own flags are just
`-config` and `-listen`).

```yaml
  "blur-detect":
    cmd: >
      /opt/homebrew/bin/llama-server
      --port 9006
      -m /Users/patrickheckenlively/models/qwen2.5-vl-7b/Qwen2.5-VL-7B-Instruct-Q8_0.gguf
      --mmproj /Users/patrickheckenlively/models/qwen2.5-vl-7b/mmproj-BF16.gguf
      --ctx-size 8192
      -ngl 99
      --jinja
      --alias blur-detect
    proxy: http://127.0.0.1:9006
    checkEndpoint: /health
    ttl: 3600
```

## 5. API call (through llama-swap :8080)

```bash
B64=$(base64 -i shot.jpg)
curl -s http://localhost:8080/v1/chat/completions -H 'Content-Type: application/json' -d @- <<EOF
{"model":"blur-detect","max_tokens":200,"temperature":0,
 "messages":[{"role":"user","content":[
   {"type":"text","text":"Is the main subject of this photo in focus or blurry? Check defocus AND motion blur. Reply with JSON only: {\"subject_blurry\":bool,\"confidence\":0-100,\"blur_type\":\"none|defocus|motion|both\",\"reason\":\"...\"}"},
   {"type":"image_url","image_url":{"url":"data:image/jpeg;base64,$B64"}}]}]}
EOF
```

**AF-box prompt addition (implemented in `internal/afbox`):** when the
EXIF carries a usable AF box, a sentence is appended to the prompt giving
the box as a pixel rectangle **in the sent JPEG's coordinates** (the box
is scaled from the camera's AF space to the resized JPEG — the VLM never
sees full-res pixels), plus a coarse region/size label, plus the
instruction to judge only the subject inside (or nearest to) the box and
ignore subjects outside it. Whole-frame AF / MF / missing data → plain
prompt. The box is echoed in the progress line and report
(`af_box: "x,y,w,h"`) for spot-checking.

## 6. Go tool design (planned)

```
cr3blur/
  main.go    arg parsing, dir walk, worker pool (4), progress, JSON report
  cr3.go     os/exec sips → resized JPEG bytes (only external dependency)
  sharp.go   optional free triage: Laplacian variance on decoded JPEG
             (stdlib image + golang.org/x/image/draw)
  vllm.go    HTTP client → llama-swap :8080, base64 image, strict-JSON prompt, retry
```

- Two-stage option: Laplacian triage (free, in-process) catches gross blur;
  VLM is the authoritative judge for the ambiguous band / subject-level blur.
- Concurrency: 4 workers. sips parallelizes; the VLM is serialized by
  llama-server.

## 7. Performance (estimates, M2 Max)

- sips develop + resize: ~2.5–3 s per CR3 (parallel across workers)
- VLM 7B: ~1–4 s per image (image prefill dominates)
- Bottleneck is the VLM → **~20–30 photos/min**; a 500-shot folder ≈ 20 min.

## 8. Caveats

- VLMs are not calibrated meters: borderline shots may flip-flop;
  confidence is self-reported. Use a threshold + spot-check flagged set.
- Downscale to ~1024 px before sending — big speedup, negligible effect on
  blur judgment.
- HEIC/JPG folders: skip sips, send the raster directly (llama.cpp loads
  jpg/png/webp/bmp/gif; not HEIC).
- If raw-domain analysis is ever wanted (sensor data, pre-sharpening, to
  separate blur from noise): libraw via cgo (libraw already installed).
  Not needed for the VLM culling path.

## 9. Next steps

1. `hf download` the Q8_0 model + mmproj-BF16 (§4)
2. Add the `blur-detect` entry to `~/.config/llama-swap/config.yaml`, restart llama-swap
3. Smoke-test one CR3 end-to-end (sips → curl → verdict)
4. Scaffold `cr3blur` Go module; run against
   `~/PhotoOnDeck/2026-10-03-RoseHomecoming` to calibrate prompt + threshold
