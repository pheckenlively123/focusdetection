# Phase 02 — Core pipeline: CR3 → VLM verdict → XMP sidecar

Goal: `cr3blur [flags] DIR` runs end-to-end — develops each CR3 to a
resized JPEG, gets a focus verdict from the VLM via llama-swap, and
writes an XMP sidecar (`<base>.xmp`) next to each CR3 recording it.

Scaffold from Phase 01 is in place (`cmd/cr3blur`, `internal/{cr3,vllm,sharp,xmp}`).
Design background: `ai-blur-detect.md`.

## Status (2026-10-17)

- Steps 1–4 implemented; `go build ./... && go vet ./... && go test ./...` clean.
  Tests: xmp (packet shape, well-formedness, splice/merge, no-clobber),
  vllm (verdict parsing), cr3 (sips plumbing).
- Full 184-file run on `~/PhotoOnDeck/2026-10-03-RoseHomecoming` done:
  184/184 sidecars, well-formed, namespace present. Verdicts:
  **179 blurry / 5 sharp / 0 failed**. Confidence pattern: all blurry at
  70–80, all sharp at 90 — the model reads "slightly soft" (crowd,
  shallow DoF) as blurry; 179/184 is suspect → calibration below is the
  next real step.
- Deviations from the plan, both driven by observed behavior:
  - `vllm.Warmup` does a minimal chat completion, not just GET /health —
    llama-swap loads models lazily on first request (health only checks
    the proxy; model port is refused until then).
  - `requestTimeout` 120 s → 300 s: llama-swap is a single slot shared
    with opencode (qwen3.8-27b); concurrent use evicts blur-detect
    mid-run and a full swap cycle can exceed 120 s (first full run: 24
    timeouts, all fixed by re-run). **Operational rule: don't run
    long cr3blur batches while opencode is active.**
- Remaining (Test & acceptance):
  1. **Calibration**: hand-label ~20 shots from the folder (sharp /
     blurry-subject / motion), compare against the report, adjust prompt
     / confidence threshold (§9.4). Merged per-file verdicts are in
     `/tmp/cr3blur-2026-10-03-rosehomecoming.json` (184 entries).
  2. Re-measure clean throughput without swap thrash (first pass was
     184 files / 1h03m with thrash; fast phase ran ~10–12 files/min).

## Prerequisites (outside this repo)

1. Download model: `hf download unsloth/Qwen2.5-VL-7B-Instruct-GGUF` (Q8_0 + mmproj-BF16, §4 of ai-blur-detect.md)
2. Add the `blur-detect` entry to `~/.config/llama-swap/config.yaml`, restart llama-swap
3. Smoke-test one CR3 by hand: sips → curl → verdict (§5)

## Step 1 — `internal/cr3`: sips develop

```go
// Develop develops a CR3 to JPEG, capped at maxDim on the long edge
// (sips -Z, aspect preserved), and returns the JPEG bytes.
func Develop(ctx context.Context, cr3Path string, maxDim int) ([]byte, error)
```

- `os/exec` → `sips -s format jpeg -Z <maxDim> <in>.CR3 --out <tmp>.jpg`
- Output to `os.MkdirTemp` file; read bytes; defer cleanup.
- `exec.CommandContext` with a 60 s timeout (sips takes ~2.5–3 s; timeout is a safety net).
- Wrap errors with the CR3 base name and captured stderr (sips writes diagnostics to stderr).
- Verified behavior (ai-blur-detect.md §3): 6000×4000 → 1024×682 in ~2.8 s.

## Step 2 — `internal/vllm`: llama-swap client

```go
type Verdict struct {
    SubjectBlurry bool
    Confidence    int    // 0-100, self-reported
    BlurType      string // none|defocus|motion|both
    Reason        string
}

// Ask sends the JPEG to the VLM and returns the parsed verdict.
func Ask(ctx context.Context, base, model string, jpeg []byte) (Verdict, error)
```

- `POST <base>/v1/chat/completions`, `model: "blur-detect"`, `temperature: 0`, `max_tokens: 200`.
- Message content: `[{"type":"text","text":<strict-JSON prompt>}, {"type":"image_url","image_url":{"url":"data:image/jpeg;base64,..."}}]` (prompt text per §5).
- Parse `choices[0].message.content` as strict JSON into `Verdict`; defensively strip markdown code fences if the model wraps them. Validate `BlurType` against the enum; reject out-of-range confidence.
- Retry: 3 attempts, backoff 1 s / 5 s, on network errors and HTTP 5xx only; 4xx is fatal. Per-request timeout 120 s (image prefill is 1–4 s, but the first request may wait on model load in llama-server).
- Warmup: `GET <base>/health` (or `/v1/models`) once before the worker pool starts, so model load happens outside the per-file timing.

## Step 3 — `internal/xmp`: sidecar writer

One sidecar per CR3: `O47A0217.CR3` → `O47A0217.xmp` (same directory, same base name — the Lightroom sidecar convention for RAW).

- Custom namespace, prefix `cr3blur:`, URI `http://github.com/pheckenlively123/focusdetection/xmp/1.0/`
- Properties (one `rdf:Description`):
  - `cr3blur:SubjectBlurry` — `true`/`false`
  - `cr3blur:Confidence` — `0`–`100`
  - `cr3blur:BlurType` — `none|defocus|motion|both`
  - `cr3blur:Reason` — free text (escape XML entities)
  - `cr3blur:SourceFile` — CR3 base name
  - `cr3blur:Model` — model alias (`blur-detect`)
  - `cr3blur:AnalyzedAt` — ISO 8601 UTC
- Packet shape: `<?xpacket begin="…" id="W5M0MpCehiHzreSzNTczkc9d"?>` + `x:xmpmeta`/`rdf:RDF` + `<?xpacket end="w"?>`, with the UTF-8 BOM.
- Existing sidecars: do **not** clobber third-party XMP (Lightroom ratings live here).
  - No sidecar → write our full packet.
  - Sidecar with only our namespace → rewrite it.
  - Sidecar with other namespaces → string-level splice: replace/insert our `<rdf:Description>` inside the existing `<rdf:RDF>` (no XML library needed; full RDF merge is a later enhancement).
- API: `WriteSidecar(ctx, cr3Path string, v vllm.Verdict, model string) error` — takes the CR3 path and derives the `.xmp` path (keeps `internal/xmp` free of a dependency on `internal/vllm` by accepting a small local verdict type or plain fields — decide at implementation time, plain fields preferred).

## Step 4 — wire `processFile` + CLI polish

- `processFile`: `cr3.Develop` → `vllm.Ask` → `xmp.WriteSidecar`.
- New flags:
  - `-size` (max dim for VLM, default `1024`)
  - `-model` (default `blur-detect`)
  - `-force` (re-analyze even if a sidecar with our namespace exists)
- Resume: skip files whose sidecar already carries our namespace (unless `-force`) — matters for 500-shot folders and crash recovery.
- Progress: one line per completed file — `12/184  O47A0217.CR3  SHARP (conf 92)`.
- Report: JSON summary at the end (per-file verdict + counts of sharp/blurry/failed); stdout, or `-report <file>`.
- Exit codes: `0` all ok, `1` fatal usage/env error, `2` completed with per-file failures.

## Step 5 (optional, deferred) — `internal/sharp` triage

Laplacian-variance pre-pass to skip the VLM on grossly blurry shots.
Only worth it if Phase 03 calibration shows the VLM-only throughput is
too slow. Design: variance threshold with an ambiguous band; VLM is
still the authoritative judge for the band.

## Test & acceptance

- Labeled sample: hand-label ~20 shots from `~/PhotoOnDeck/2026-10-03-RoseHomecoming` (sharp / blurry-subject / motion), run the tool, compare verdicts. This is the prompt + confidence-threshold calibration step (ai-blur-detect.md §9.4).
- Sidecar check: `.xmp` files exist, XML is well-formed, our namespace present; re-run with mixed sidecars (add a fake third-party `lr:Description` to one) and confirm merge, not clobber.
- Resume: run a 30-file subset, kill mid-run, re-run — only unfinished files are processed.
- Throughput: measure on the full 184-file folder against the 20–30 photos/min estimate (§7).
