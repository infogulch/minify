package minify

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"unicode/utf8"
)

// SourceMapOptions configures Source Map v3 serialization.
type SourceMapOptions struct {
	// File is the optional "file" field (generated file name).
	File string
	// Source is the path recorded in "sources" (defaults to "source").
	Source string
	// SourceRoot is the optional "sourceRoot" field.
	SourceRoot string
	// IncludeContent embeds the original text in "sourcesContent".
	// Recommended for standalone visualizers.
	IncludeContent bool
}

// SourceMap returns a Source Map v3 JSON document for this index.
// original and minified must be the texts used to build the index (the Index
// retains neither). See WriteSourceMap. Panics if encoding fails (programmer error;
// writes to an in-memory buffer do not fail under normal conditions).
func (ix *Index) SourceMap(original, minified []byte, opts SourceMapOptions) []byte {
	var buf bytes.Buffer
	if err := ix.WriteSourceMap(&buf, original, minified, opts); err != nil {
		panic("minify: SourceMap: " + err.Error())
	}
	return buf.Bytes()
}

// WriteSourceMap writes a Source Map v3 JSON document to w.
//
// Columns are UTF-16 code units (Source Map v3); the Index remains byte-offset
// based. "names" is empty. Each Index segment becomes one map entry (plus an
// entry at each generated line start inside multi-line segments). Mid-segment
// columns are approximate under nearest-previous lookup; use Index.Original for
// slope-1 positions.
func (ix *Index) WriteSourceMap(w io.Writer, original, minified []byte, opts SourceMapOptions) error {
	srcName := opts.Source
	if srcName == "" {
		srcName = "source"
	}

	doc := sourceMapDoc{
		Version: 3,
		File:    opts.File,
		Sources: []string{srcName},
		Names:   []string{},
	}
	if opts.SourceRoot != "" {
		doc.SourceRoot = opts.SourceRoot
	}
	if opts.IncludeContent {
		// JSON null for missing content is allowed; we always have original when asked.
		content := string(original)
		doc.SourcesContent = []*string{&content}
	}
	if ix != nil && ix.outLen > 0 && len(ix.segs) > 0 {
		doc.Mappings = ix.encodeMappings(original, minified)
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

type sourceMapDoc struct {
	Version        int       `json:"version"`
	File           string    `json:"file,omitempty"`
	SourceRoot     string    `json:"sourceRoot,omitempty"`
	Sources        []string  `json:"sources"`
	SourcesContent []*string `json:"sourcesContent,omitempty"`
	Names          []string  `json:"names"`
	Mappings       string    `json:"mappings"`
}

// mapPoint is one Source Map segment (4 fields; no name index).
type mapPoint struct {
	genLine  int // 0-based
	genCol   int // 0-based UTF-16
	origLine int // 0-based
	origCol  int // 0-based UTF-16
}

func (ix *Index) encodeMappings(original, minified []byte) string {
	points := ix.collectMapPoints(original, minified)
	genLines := ix.resolvedGenLines(minified)
	if len(points) == 0 {
		// Still need enough ';' for generated line count so tools see line shape.
		return genLineSeparators(genLines, ix.outLen)
	}

	var b []byte
	prevGenLine := 0
	prevGenCol := 0
	prevOrigLine := 0
	prevOrigCol := 0
	prevSourceIdx := 0
	sourceIdx := 0

	for i, p := range points {
		// Close finished lines and open empty ones between prev and current.
		for prevGenLine < p.genLine {
			b = append(b, ';')
			prevGenLine++
			prevGenCol = 0
		}
		if i > 0 && points[i-1].genLine == p.genLine {
			b = append(b, ',')
		}

		// 4-field segment: genCol, sourceIdx, origLine, origCol (all relative).
		b = appendVLQ(b, p.genCol-prevGenCol)
		b = appendVLQ(b, sourceIdx-prevSourceIdx)
		b = appendVLQ(b, p.origLine-prevOrigLine)
		b = appendVLQ(b, p.origCol-prevOrigCol)

		prevGenCol = p.genCol
		prevSourceIdx = sourceIdx
		prevOrigLine = p.origLine
		prevOrigCol = p.origCol
	}

	// Trailing empty lines through last generated line.
	lastLine := lastGenLineIndex(genLines, ix.outLen)
	for prevGenLine < lastLine {
		b = append(b, ';')
		prevGenLine++
	}
	return string(b)
}

// resolvedGenLines prefers a definitive scan of minified when it matches the
// index length (guards against write-boundary quirks in incremental genLines).
func (ix *Index) resolvedGenLines(minified []byte) []int {
	if ix == nil {
		return nil
	}
	if len(minified) == ix.outLen && len(minified) > 0 {
		return lineStarts(minified)
	}
	if len(ix.genLines) > 0 {
		return ix.genLines
	}
	if len(minified) > 0 {
		return lineStarts(minified)
	}
	return ix.genLines
}

func lastGenLineIndex(genLines []int, outLen int) int {
	if outLen <= 0 || len(genLines) == 0 {
		return 0
	}
	lastLine := len(genLines) - 1
	// Trailing newline → last start is past content; source-map line count
	// is the number of lines in the file.
	if genLines[lastLine] >= outLen && lastLine > 0 {
		lastLine--
	}
	return lastLine
}

func genLineSeparators(genLines []int, outLen int) string {
	if outLen == 0 || len(genLines) == 0 {
		return ""
	}
	// genLines[0]==0; remaining entries are starts of subsequent lines.
	// Number of ';' between lines = max(0, lineCount-1).
	lineCount := len(genLines)
	if genLines[lineCount-1] >= outLen && lineCount > 1 {
		lineCount-- // trailing newline → last start is past end
	}
	if lineCount <= 1 {
		return ""
	}
	return string(bytes.Repeat([]byte{';'}, lineCount-1))
}

func (ix *Index) collectMapPoints(original, minified []byte) []mapPoint {
	if ix == nil || len(ix.segs) == 0 {
		return nil
	}
	// Prefer definitive scans of the provided texts when lengths match the
	// index (Source Map serialization always has both). Fall back to stored
	// tables or a scan when the index tables are empty.
	origLines := ix.origLines
	if len(original) == ix.srcLen && len(original) > 0 {
		origLines = lineStarts(original)
	} else if len(origLines) == 0 && len(original) > 0 {
		origLines = lineStarts(original)
	}
	genLines := ix.resolvedGenLines(minified)

	// One map point per segment start, plus gen line starts inside multi-line segs.
	points := make([]mapPoint, 0, len(ix.segs)*2)
	for _, s := range ix.segs {
		if s.length <= 0 {
			continue
		}
		points = append(points, mapPointAt(minified, original, genLines, origLines, s.gen, s.orig))
		segEnd := s.gen + s.length
		for _, ls := range genLines {
			if ls <= s.gen {
				continue
			}
			if ls >= segEnd {
				break
			}
			o := s.orig
			if s.exact {
				o = s.orig + (ls - s.gen)
			}
			points = append(points, mapPointAt(minified, original, genLines, origLines, ls, o))
		}
	}
	return points
}

func mapPointAt(minified, original []byte, genLines, origLines []int, genOff, origOff int) mapPoint {
	gl, gc := offsetToLineUTF16Col(minified, genLines, genOff)
	ol, oc := offsetToLineUTF16Col(original, origLines, origOff)
	return mapPoint{genLine: gl, genCol: gc, origLine: ol, origCol: oc}
}

// offsetToLineUTF16Col returns 0-based line and UTF-16 code-unit column for a
// byte offset into text. lineStarts must list byte offsets of each line start.
func offsetToLineUTF16Col(text []byte, lineStarts []int, offset int) (line0, colUTF16 int) {
	if offset < 0 {
		offset = 0
	}
	if len(text) > 0 && offset > len(text) {
		offset = len(text)
	}
	if len(lineStarts) == 0 {
		return 0, utf16Count(text[:min(offset, len(text))])
	}
	// Largest lineStarts[i] <= offset.
	i := sort.Search(len(lineStarts), func(j int) bool {
		return lineStarts[j] > offset
	}) - 1
	if i < 0 {
		i = 0
	}
	start := lineStarts[i]
	if start > len(text) {
		start = len(text)
	}
	end := offset
	if end > len(text) {
		end = len(text)
	}
	if end < start {
		end = start
	}
	return i, utf16Count(text[start:end])
}

// utf16Count returns the number of UTF-16 code units in b.
func utf16Count(b []byte) int {
	n := 0
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			n++ // treat invalid byte as one unit
			i++
			continue
		}
		if r <= 0xFFFF {
			n++
		} else {
			n += 2 // surrogate pair
		}
		i += size
	}
	return n
}

// base64 VLQ alphabet (Source Map v3).
const vlqBase64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// appendVLQ appends a Base64 VLQ-encoded signed integer (Source Map style).
// Works correctly for mapped sources up to 2^31 bytes in length.
func appendVLQ(dst []byte, v int) []byte {
	var u uint32
	if v < 0 {
		u = uint32((-v)<<1) | 1
	} else {
		u = uint32(v << 1)
	}
	for {
		digit := u & 31
		u >>= 5
		if u > 0 {
			digit |= 32 // continuation
		}
		dst = append(dst, vlqBase64[digit])
		if u == 0 {
			break
		}
	}
	return dst
}
