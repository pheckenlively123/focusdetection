// Package sharp provides an optional free triage pass: a Laplacian
// variance sharpness metric on the decoded JPEG (stdlib image +
// golang.org/x/image/draw). It catches gross blur in-process; the VLM
// (internal/vllm) remains the authoritative judge for subject-level
// focus.
package sharp
