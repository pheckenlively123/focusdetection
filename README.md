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
.CR3 ──sips──▶ JPEG (≤1024 px) ──base64──▶ llama-server :9006
                                            (Qwen2.5-VL-7B + mmproj)
report ◀── JSON verdicts ◀───────────────────────────────────┘
sidecar ◀── .xmp (cr3blur: namespace) ◀────────────────────────┘
```

By default `cr3blur` talks directly to the llama-server port that the
llama-swap config assigns to the `blur-detect` model (9006). Pass
`-url http://127.0.0.1:8080` to route through the llama-swap listener
instead (needed if you want llama-swap to lazy-load/swap the model).
Note the model port refuses connections until llama-swap has loaded the
model, so with the default direct URL the model must already be loaded.

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

No external Go dependencies — stdlib plus `os/exec` for `sips`.

## Usage

```
cr3blur [flags] DIR
```

Walks `DIR` recursively for `.CR3` files, and for each one:
develops it to a resized JPEG, asks the VLM for a verdict, and writes an XMP
sidecar.

| Flag | Default | Description |
|---|---|---|
| `-url` | `http://127.0.0.1:9006` | OpenAI-compatible endpoint — default: the `blur-detect` llama-server port from the llama-swap config; or the llama-swap listener (e.g. `http://127.0.0.1:8080`) |
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
  per-file timing. Pointed at the llama-swap listener, this also triggers the
  lazy model load; with the default direct llama-server URL it only verifies
  the model is already up.
- **Resume**: files whose sidecar already carries our namespace are skipped
  (use `-force` to re-analyze) — matters for 500-shot folders and crash
  recovery.
- **Progress**: one line per completed file:

  ```
  12/184  O47A0217.CR3  ★★ SHARP (conf 92)
  13/184  O47A0218.CR3  ★ BLURRY (defocus, conf 71)
  14/184  O47A0219.CR3  FAILED: sips: exit status 1
  ```

  The star(s) before the status are the rating written to the sidecar
  (2 = in focus, 1 = blurry).

- **Report**: a JSON summary (per-file verdicts, star ratings, and
  sharp/blurry/failed counts) printed to stdout or written to `-report FILE`.
- **Exit codes**: `0` all ok, `1` fatal usage/env error, `2` completed with
  per-file failures.

### The prompt

Each image is sent with a strict-JSON prompt asking the model to check
defocus **and** motion blur and reply with a single JSON object:

```json
{"subject_blurry":bool,"confidence":0-100,"blur_type":"none|defocus|motion|both","reason":"..."}
```

The built-in prompt can be overridden with `-prompt TEXT` — handy for
prompt calibration (e.g. teaching the model to ignore background softness
or shallow-DoF bokeh). The response is still parsed into the fixed JSON
schema above, so a custom prompt must ask the model to reply with the
same object, or every file fails verdict parsing.

The response is parsed defensively (markdown fences / stray prose tolerated),
validated against the `blur_type` enum and the 0–100 confidence range, and
retried (1 s / 5 s backoff) on network errors and HTTP 5xx only.

## XMP sidecars

One sidecar per CR3, using the Lightroom RAW sidecar convention:
`O47A0217.CR3` → `O47A0217.xmp` (same directory, same base name).

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
verdict parsing (fenced, prose-wrapped, invalid), and the sips plumbing.

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
  With the default direct model-port URL, an eviction surfaces as
  connection-refused on subsequent requests.

## License

GPL v3 — see [LICENSE](LICENSE).
