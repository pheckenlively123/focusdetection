package xmp

import (
	"context"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertWellFormed decodes the whole document; any XML syntax error
// fails the test. (A leading UTF-8 BOM is handled by the decoder.)
func assertWellFormed(t *testing.T, s string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(s))
	for {
		if _, err := dec.Token(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("XML not well-formed: %v", err)
		}
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteSidecarNew(t *testing.T) {
	cr3Path := filepath.Join(t.TempDir(), "O47A0217.CR3")
	if err := WriteSidecar(context.Background(), cr3Path, true, 71, "defocus",
		`blur <subject> & "motion"`, "blur-detect"); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, SidecarPath(cr3Path))

	if !strings.HasPrefix(got, "\uFEFF<?xpacket begin=\"\uFEFF\" id=\"W5M0MpCehiHzreSzNTczkc9d\"?>") {
		t.Errorf("bad packet header: %q", got[:min(80, len(got))])
	}
	if !strings.HasSuffix(got, `<?xpacket end="w"?>`) {
		t.Error("missing <?xpacket end=...?> trailer")
	}
	for _, want := range []string{
		nsURI,
		`<cr3blur:SubjectBlurry>true</cr3blur:SubjectBlurry>`,
		`<cr3blur:Confidence>71</cr3blur:Confidence>`,
		`<cr3blur:BlurType>defocus</cr3blur:BlurType>`,
		`<cr3blur:Reason>blur &lt;subject&gt; &amp; &quot;motion&quot;</cr3blur:Reason>`,
		`<cr3blur:SourceFile>O47A0217.CR3</cr3blur:SourceFile>`,
		`<cr3blur:Model>blur-detect</cr3blur:Model>`,
		`<cr3blur:AnalyzedAt>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	assertWellFormed(t, got)

	has, err := HasVerdict(cr3Path)
	if err != nil || !has {
		t.Errorf("HasVerdict = %v, %v; want true, nil", has, err)
	}
}

func TestWriteSidecarNoClobber(t *testing.T) {
	cr3Path := filepath.Join(t.TempDir(), "O47A0217.CR3")
	sidecar := SidecarPath(cr3Path)
	// A Lightroom-style sidecar with a third-party namespace.
	thirdParty := "\uFEFF<?xpacket begin=\"\uFEFF\" id=\"W5M0MpCehiHzreSzNTczkc9d\"?>\n" +
		`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="Adobe XMP Core 7.2">` + "\n" +
		`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` + "\n" +
		` <rdf:Description rdf:about="" xmlns:lr="http://ns.adobe.com/lightroom/1.0/">` + "\n" +
		`  <lr:Rating>4</lr:Rating>` + "\n" +
		` </rdf:Description>` + "\n" +
		`</rdf:RDF>` + "\n" +
		`</x:xmpmeta>` + "\n" +
		`<?xpacket end="w"?>`
	if err := os.WriteFile(sidecar, []byte(thirdParty), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteSidecar(context.Background(), cr3Path, false, 92, "none", "in focus", "blur-detect"); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, sidecar)
	if !strings.Contains(got, `<lr:Rating>4</lr:Rating>`) {
		t.Errorf("third-party property lost:\n%s", got)
	}
	if !strings.Contains(got, `<cr3blur:SubjectBlurry>false</cr3blur:SubjectBlurry>`) {
		t.Errorf("our property missing:\n%s", got)
	}
	if n := strings.Count(got, "xmlns:cr3blur"); n != 1 {
		t.Errorf("xmlns:cr3blur count = %d, want 1", n)
	}
	assertWellFormed(t, got)

	// Re-writing must replace our block, not duplicate it.
	if err := WriteSidecar(context.Background(), cr3Path, true, 55, "motion", "moved", "blur-detect"); err != nil {
		t.Fatal(err)
	}
	got = mustRead(t, sidecar)
	if n := strings.Count(got, "xmlns:cr3blur"); n != 1 {
		t.Errorf("after re-write, xmlns:cr3blur count = %d, want 1", n)
	}
	if !strings.Contains(got, `<lr:Rating>4</lr:Rating>`) {
		t.Errorf("third-party property lost on re-write:\n%s", got)
	}
	if !strings.Contains(got, `<cr3blur:BlurType>motion</cr3blur:BlurType>`) {
		t.Errorf("re-written verdict missing:\n%s", got)
	}
	assertWellFormed(t, got)
}

func TestWriteSidecarRewriteOursOnly(t *testing.T) {
	cr3Path := filepath.Join(t.TempDir(), "O47A0217.CR3")
	if err := WriteSidecar(context.Background(), cr3Path, false, 50, "none", "old", "blur-detect"); err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecar(context.Background(), cr3Path, true, 88, "both", "new", "blur-detect"); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, SidecarPath(cr3Path))
	if !strings.Contains(got, `<cr3blur:Reason>new</cr3blur:Reason>`) || strings.Contains(got, `>old<`) {
		t.Errorf("stale content survived rewrite:\n%s", got)
	}
	if n := strings.Count(got, "<rdf:Description"); n != 1 {
		t.Errorf("rdf:Description count = %d, want 1", n)
	}
	assertWellFormed(t, got)
}

func TestWriteSidecarRefusesUnrecognized(t *testing.T) {
	cr3Path := filepath.Join(t.TempDir(), "O47A0217.CR3")
	sidecar := SidecarPath(cr3Path)
	if err := os.WriteFile(sidecar, []byte("not xml at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecar(context.Background(), cr3Path, false, 50, "none", "x", "blur-detect"); err == nil {
		t.Fatal("expected error for unrecognized sidecar, got nil")
	}
	if got := mustRead(t, sidecar); got != "not xml at all" {
		t.Errorf("unrecognized sidecar was modified: %q", got)
	}
}

func TestSidecarPath(t *testing.T) {
	got := SidecarPath("/a/b/O47A0217.CR3")
	if want := "/a/b/O47A0217.xmp"; got != want {
		t.Errorf("SidecarPath = %q, want %q", got, want)
	}
}
