// Package vllm is the HTTP client for the vision LLM served through
// llama-swap (OpenAI-compatible POST /v1/chat/completions). It sends
// a resized JPEG as a base64 data URL with a strict-JSON prompt and
// retries on transient errors.
package vllm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Verdict is the focus verdict for one photo, as reported by the VLM.
type Verdict struct {
	SubjectBlurry bool
	Confidence    int    // 0-100, self-reported
	BlurType      string // none|defocus|motion|both
	Reason        string
}

// Rating is the star rating assigned to the photo for culling:
// 1 star when the subject is blurry, 2 stars when it is in focus.
// It is written to the sidecar as the standard xmp:Rating so culling
// tools (e.g. Lightroom) display the stars.
func (v Verdict) Rating() int {
	if v.SubjectBlurry {
		return 1
	}
	return 2
}

// DefaultPrompt is the strict-JSON instruction sent with each image
// (ai-blur-detect.md §5) unless the caller overrides it (cr3blur
// -prompt). The model must answer with a single JSON object and
// nothing else; the response is still parsed into the fixed verdict
// schema, so a custom prompt must ask for the same JSON fields.
const DefaultPrompt = `Is the main subject of this photo in focus or blurry? Check defocus AND motion blur. Reply with JSON only: {"subject_blurry":bool,"confidence":0-100,"blur_type":"none|defocus|motion|both","reason":"..."}`

const (
	// requestTimeout bounds a single request. Image prefill is 1–4 s,
	// but a request can also wait on a model load/swap in llama-swap:
	// the server is a single-slot shared with other llama-swap clients,
	// and a full swap cycle (unload + load of the other model) measured
	// up to ~90 s. 120 s (the original estimate) was observed to time
	// out mid-swap on real runs; 300 s rides out a swap instead.
	requestTimeout = 300 * time.Second
	maxAttempts    = 3
)

// backoff before attempt 2 and attempt 3 (on retryable errors only).
var backoffs = []time.Duration{1 * time.Second, 5 * time.Second}

// validBlurTypes is the closed set of BlurType values.
var validBlurTypes = map[string]bool{"none": true, "defocus": true, "motion": true, "both": true}

type chatRequest struct {
	Model       string    `json:"model"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	Messages    []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content []part `json:"content"`
}

type part struct {
	Type     string  `json:"type"`
	Text     string  `json:"text,omitempty"`
	ImageURL *imgURL `json:"image_url,omitempty"`
}

type imgURL struct {
	URL string `json:"url"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Ask sends jpeg to the VLM with the given text prompt and returns
// the parsed verdict. Use DefaultPrompt for the stock blur prompt.
//
// Retries with 1 s / 5 s backoff on network errors and HTTP 5xx;
// HTTP 4xx and verdict-parse errors are fatal for the caller.
func Ask(ctx context.Context, base, model, prompt string, jpeg []byte) (Verdict, error) {
	if strings.TrimSpace(prompt) == "" {
		return Verdict{}, fmt.Errorf("vllm: empty prompt")
	}
	body, err := json.Marshal(chatRequest{
		Model:       model,
		Temperature: 0,
		MaxTokens:   200,
		Messages: []message{{
			Role: "user",
			Content: []part{
				{Type: "text", Text: prompt},
				{Type: "image_url", ImageURL: &imgURL{
					URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg),
				}},
			},
		}},
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("vllm: encode request: %w", err)
	}

	client := &http.Client{Timeout: requestTimeout}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return Verdict{}, ctx.Err()
			case <-time.After(backoffs[attempt-2]):
			}
		}
		v, retryable, err := askOnce(ctx, client, base, body)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if !retryable {
			return Verdict{}, err
		}
	}
	return Verdict{}, fmt.Errorf("vllm: %s: failed after %d attempts: %w", model, maxAttempts, lastErr)
}

// askOnce performs a single request. The second return value says
// whether the error is worth retrying (network failure or 5xx).
func askOnce(ctx context.Context, client *http.Client, base string, body []byte) (Verdict, bool, error) {
	url := strings.TrimSuffix(base, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Verdict{}, false, fmt.Errorf("vllm: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return Verdict{}, true, fmt.Errorf("vllm: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Verdict{}, true, fmt.Errorf("vllm: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode >= 500
		detail := strings.TrimSpace(string(raw))
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		return Verdict{}, retryable, fmt.Errorf("vllm: HTTP %d: %s", resp.StatusCode, detail)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Verdict{}, false, fmt.Errorf("vllm: decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return Verdict{}, false, fmt.Errorf("vllm: empty response (no choices)")
	}
	return parseVerdict(cr.Choices[0].Message.Content)
}

// rawVerdict mirrors the model's snake_case JSON. Confidence is a
// pointer so a missing field is an error rather than a silent zero.
type rawVerdict struct {
	SubjectBlurry *bool   `json:"subject_blurry"`
	Confidence    *int    `json:"confidence"`
	BlurType      *string `json:"blur_type"`
	Reason        string  `json:"reason"`
}

// parseVerdict parses the model's content string as strict JSON,
// tolerating a markdown code fence or stray prose around the object.
func parseVerdict(content string) (Verdict, bool, error) {
	s := stripFences(content)
	if i := strings.IndexByte(s, '{'); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndexByte(s, '}'); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}
	var rv rawVerdict
	if err := json.Unmarshal([]byte(s), &rv); err != nil {
		return Verdict{}, false, fmt.Errorf("vllm: verdict is not valid JSON: %q", truncate(content, 300))
	}
	if rv.SubjectBlurry == nil || rv.Confidence == nil || rv.BlurType == nil {
		return Verdict{}, false, fmt.Errorf("vllm: verdict missing field(s): %s", truncate(content, 300))
	}
	if !validBlurTypes[*rv.BlurType] {
		return Verdict{}, false, fmt.Errorf("vllm: invalid blur_type %q (want none|defocus|motion|both)", *rv.BlurType)
	}
	if *rv.Confidence < 0 || *rv.Confidence > 100 {
		return Verdict{}, false, fmt.Errorf("vllm: confidence %d out of range 0-100", *rv.Confidence)
	}
	return Verdict{
		SubjectBlurry: *rv.SubjectBlurry,
		Confidence:    *rv.Confidence,
		BlurType:      *rv.BlurType,
		Reason:        rv.Reason,
	}, false, nil
}

// stripFences removes a markdown code fence (```json … ```) if the
// model wrapped its JSON in one.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	} else {
		s = strings.TrimPrefix(s, "```")
	}
	if strings.HasSuffix(strings.TrimSpace(s), "```") {
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Warmup checks the endpoint and triggers model load/swap before the
// worker pool runs, keeping load time out of the per-file timing.
//
// Note: a GET /health on llama-swap only checks the proxy itself —
// models load lazily on the first request for their alias (verified:
// the model port is refused until then, first completion ~9 s).
// So step 2 sends a minimal text-only completion against the alias,
// which loads (or swaps in) the model and validates the alias.
func Warmup(ctx context.Context, base, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	client := &http.Client{}
	root := strings.TrimSuffix(base, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("vllm: warmup: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vllm: warmup: %s: HTTP %d", root+"/health", resp.StatusCode)
	}

	body, err := json.Marshal(chatRequest{
		Model:       model,
		Temperature: 0,
		MaxTokens:   1,
		Messages:    []message{{Role: "user", Content: []part{{Type: "text", Text: "ping"}}}},
	})
	if err != nil {
		return err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, root+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("vllm: warmup: load model %q: %w", model, err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("vllm: warmup: load model %q: %w", model, err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(raw))
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		return fmt.Errorf("vllm: warmup: load model %q: HTTP %d: %s", model, resp.StatusCode, detail)
	}
	return nil
}
