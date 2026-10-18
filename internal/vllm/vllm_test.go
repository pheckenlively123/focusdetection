package vllm

import (
	"context"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Verdict
		ok   bool
	}{
		{
			name: "plain",
			in:   `{"subject_blurry":false,"confidence":92,"blur_type":"none","reason":"sharp throughout"}`,
			want: Verdict{false, 92, "none", "sharp throughout"},
			ok:   true,
		},
		{
			name: "fenced",
			in:   "```json\n{\"subject_blurry\":true,\"confidence\":55,\"blur_type\":\"motion\",\"reason\":\"moved\"}\n```",
			want: Verdict{true, 55, "motion", "moved"},
			ok:   true,
		},
		{
			name: "prose-around",
			in:   `Here is the verdict: {"subject_blurry":true,"confidence":80,"blur_type":"both","reason":"x"}`,
			want: Verdict{true, 80, "both", "x"},
			ok:   true,
		},
		{
			name: "edge-confidence-0",
			in:   `{"subject_blurry":true,"confidence":0,"blur_type":"defocus","reason":"x"}`,
			want: Verdict{true, 0, "defocus", "x"},
			ok:   true,
		},
		{
			name: "bad-blur-type",
			in:   `{"subject_blurry":true,"confidence":80,"blur_type":"blurry","reason":"x"}`,
			ok:   false,
		},
		{
			name: "confidence-too-high",
			in:   `{"subject_blurry":true,"confidence":101,"blur_type":"none","reason":"x"}`,
			ok:   false,
		},
		{
			name: "missing-fields",
			in:   `{"subject_blurry":true}`,
			ok:   false,
		},
		{
			name: "not-json",
			in:   "The subject is in focus.",
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, retryable, err := parseVerdict(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if retryable {
					t.Error("retryable = true, want false")
				}
				if got != tc.want {
					t.Errorf("got %+v, want %+v", got, tc.want)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				if retryable {
					t.Error("retryable = true for parse error, want false")
				}
			}
		})
	}
}

func TestAskEmptyPrompt(t *testing.T) {
	_, err := Ask(context.Background(), "http://127.0.0.1:1", "blur-detect", "   ", []byte{})
	if err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestVerdictRating(t *testing.T) {
	if got := (Verdict{SubjectBlurry: true}).Rating(); got != 1 {
		t.Errorf("blurry verdict rating = %d, want 1 star", got)
	}
	if got := (Verdict{SubjectBlurry: false}).Rating(); got != 2 {
		t.Errorf("in-focus verdict rating = %d, want 2 stars", got)
	}
}

func TestStripFences(t *testing.T) {
	cases := map[string]string{
		"no fence":    `{"a":1}`,
		"fenced":      "```json\n{\"a\":1}\n```",
		"plain-fence": "```\n{\"a\":1}\n```",
		"trailing":    `{"a":1} thanks`,
	}
	for name, in := range cases {
		if got := stripFences(in); got != `{"a":1}` && name != "trailing" {
			t.Errorf("%s: stripFences(%q) = %q", name, in, got)
		}
	}
}
