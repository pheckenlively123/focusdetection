// Package xmp reads and writes XMP sidecar files — a .xmp file with
// the same base name as each .CR3 (the Lightroom RAW sidecar
// convention) — recording the in-focus verdict (subject_blurry,
// confidence, blur_type, reason) so the result survives into culling
// tools that read XMP sidecars.
//
// Our verdict lives in a custom namespace (cr3blur:,
// http://github.com/pheckenlively123/focusdetection/xmp/1.0/) so it
// can coexist with third-party properties (e.g. Lightroom ratings) in
// the same sidecar. Existing sidecars are never clobbered: files that
// carry other namespaces get our <rdf:Description> spliced in at the
// string level (no XML library; full RDF merge is a later
// enhancement).
package xmp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	nsPrefix = "cr3blur"
	nsURI    = "http://github.com/pheckenlively123/focusdetection/xmp/1.0/"
	packetID = "W5M0MpCehiHzreSzNTczkc9d"
	rdfNS    = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
	xmpNS    = "adobe:ns:meta/"
	bom      = "\uFEFF"
)

// SidecarPath returns the .xmp sidecar path for a CR3 file
// (same directory, same base name: O47A0217.CR3 → O47A0217.xmp).
func SidecarPath(cr3Path string) string {
	base := filepath.Base(cr3Path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	return filepath.Join(filepath.Dir(cr3Path), name+".xmp")
}

// HasVerdict reports whether the sidecar for cr3Path exists and
// already carries our namespace. Used for resume/skip.
func HasVerdict(cr3Path string) (bool, error) {
	b, err := os.ReadFile(SidecarPath(cr3Path))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("xmp: read %s: %w", SidecarPath(cr3Path), err)
	}
	return strings.Contains(string(b), nsURI), nil
}

// WriteSidecar records the verdict for cr3Path in its .xmp sidecar.
//
//   - no sidecar (or empty) → write a full packet with our namespace
//   - sidecar with only our namespace → rewrite it
//   - sidecar with other namespaces → splice/replace our
//     <rdf:Description> inside the existing <rdf:RDF>, preserving
//     everything else byte-for-byte
//
// The write is atomic (temp file + rename).
func WriteSidecar(ctx context.Context, cr3Path string, subjectBlurry bool, confidence int, blurType, reason, model string) error {
	sidecar := SidecarPath(cr3Path)
	existing, err := os.ReadFile(sidecar)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("xmp: read %s: %w", sidecar, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	block := descriptionBlock(subjectBlurry, confidence, blurType, reason, model, filepath.Base(cr3Path), time.Now().UTC().Format(time.RFC3339))
	var out string
	switch {
	case os.IsNotExist(err) || len(strings.TrimSpace(string(existing))) == 0:
		out = fullPacket(block)
	case strings.Contains(string(existing), nsURI) && !hasOtherNamespaces(string(existing)):
		out = fullPacket(block)
	default:
		s := replaceOurBlock(string(existing), block)
		if !strings.Contains(s, nsURI) {
			// No block of ours yet: insert before </rdf:RDF>.
			i := strings.Index(s, "</rdf:RDF>")
			if i < 0 {
				return fmt.Errorf("xmp: %s: has no </rdf:RDF>; refusing to clobber unrecognized sidecar", sidecar)
			}
			s = s[:i] + block + "\n" + s[i:]
		}
		out = s
	}

	tmp, err := os.CreateTemp(filepath.Dir(sidecar), ".cr3blur-*.tmp")
	if err != nil {
		return fmt.Errorf("xmp: create temp for %s: %w", sidecar, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		return fmt.Errorf("xmp: write %s: %w", sidecar, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("xmp: chmod %s: %w", sidecar, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("xmp: close %s: %w", sidecar, err)
	}
	if err := os.Rename(tmpName, sidecar); err != nil {
		return fmt.Errorf("xmp: rename into %s: %w", sidecar, err)
	}
	return nil
}

// descriptionBlock renders our single <rdf:Description>.
func descriptionBlock(subjectBlurry bool, confidence int, blurType, reason, model, sourceFile, analyzedAt string) string {
	p := nsPrefix
	return "<rdf:Description rdf:about=\"\" xmlns:" + p + "=\"" + nsURI + "\">" +
		"<" + p + ":SubjectBlurry>" + strconv.FormatBool(subjectBlurry) + "</" + p + ":SubjectBlurry>" +
		"<" + p + ":Confidence>" + strconv.Itoa(confidence) + "</" + p + ":Confidence>" +
		"<" + p + ":BlurType>" + xmlEscape(blurType) + "</" + p + ":BlurType>" +
		"<" + p + ":Reason>" + xmlEscape(reason) + "</" + p + ":Reason>" +
		"<" + p + ":SourceFile>" + xmlEscape(sourceFile) + "</" + p + ":SourceFile>" +
		"<" + p + ":Model>" + xmlEscape(model) + "</" + p + ":Model>" +
		"<" + p + ":AnalyzedAt>" + xmlEscape(analyzedAt) + "</" + p + ":AnalyzedAt>" +
		"</rdf:Description>"
}

// fullPacket renders a complete XMP packet: UTF-8 BOM, xpacket
// wrapper, x:xmpmeta and a single rdf:RDF holding our block.
func fullPacket(block string) string {
	var b strings.Builder
	b.WriteString(bom)
	b.WriteString("<?xpacket begin=\"")
	b.WriteString(bom)
	b.WriteString("\" id=\"")
	b.WriteString(packetID)
	b.WriteString("\"?>\n")
	b.WriteString("<x:xmpmeta xmlns:x=\"" + xmpNS + "\" x:xmptk=\"cr3blur\">\n")
	b.WriteString("<rdf:RDF xmlns:rdf=\"" + rdfNS + "\">\n")
	b.WriteString(" " + block + "\n")
	b.WriteString("</rdf:RDF>\n")
	b.WriteString("</x:xmpmeta>\n")
	b.WriteString("<?xpacket end=\"w\"?>")
	return b.String()
}

// xmlnsDecl matches xmlns:prefix="uri" declarations.
var xmlnsDecl = regexp.MustCompile(`xmlns:(\w[\w.-]*)="[^"]*"`)

// hasOtherNamespaces reports whether s declares any XML namespace
// other than rdf:, x: (the xmpmeta meta-namespace) and our own.
func hasOtherNamespaces(s string) bool {
	for _, m := range xmlnsDecl.FindAllStringSubmatch(s, -1) {
		if m[1] != "rdf" && m[1] != "x" && m[1] != nsPrefix {
			return true
		}
	}
	return false
}

// replaceOurBlock replaces every <rdf:Description> block that belongs
// to our namespace with newBlock, leaving all other blocks untouched.
func replaceOurBlock(s, newBlock string) string {
	offset := 0
	for offset < len(s) {
		rest := s[offset:]
		start := strings.Index(rest, "<rdf:Description")
		if start < 0 {
			break
		}
		start += offset
		end := strings.Index(s[start:], "</rdf:Description>")
		if end < 0 {
			break
		}
		end += start
		blockEnd := end + len("</rdf:Description>")
		block := s[start:blockEnd]
		if strings.Contains(block, nsPrefix+":") || strings.Contains(block, nsURI) {
			s = s[:start] + newBlock + s[blockEnd:]
			offset = start + len(newBlock)
		} else {
			offset = blockEnd
		}
	}
	return s
}

// xmlEscape escapes the five XML predefined entities.
func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
