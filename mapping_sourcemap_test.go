package minify

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendVLQ(t *testing.T) {
	// Common Source Map VLQ test vectors.
	cases := []struct {
		v    int
		want string
	}{
		{0, "A"},
		{1, "C"},
		{-1, "D"},
		{2, "E"},
		{15, "e"},
		{16, "gB"},
		{1024, "ggC"},
	}
	for _, tc := range cases {
		got := string(appendVLQ(nil, tc.v))
		if got != tc.want {
			t.Errorf("appendVLQ(%d)=%q want %q", tc.v, got, tc.want)
		}
		n, rest := decodeVLQ([]byte(got))
		if n != tc.v || len(rest) != 0 {
			t.Errorf("decode VLQ(%d): got %d rest %q", tc.v, n, rest)
		}
	}
}

func TestUTF16Count(t *testing.T) {
	if n := utf16Count([]byte("abc")); n != 3 {
		t.Errorf("ascii: %d", n)
	}
	// U+1F600 😀 is one rune, two UTF-16 code units.
	if n := utf16Count([]byte("a😀b")); n != 4 {
		t.Errorf("emoji: %d want 4", n)
	}
}

func TestOffsetToLineUTF16Col(t *testing.T) {
	text := []byte("ab\nc😀d")
	starts := lineStarts(text)
	line, col := offsetToLineUTF16Col(text, starts, 0)
	if line != 0 || col != 0 {
		t.Errorf("off0: %d:%d", line, col)
	}
	line, col = offsetToLineUTF16Col(text, starts, 3) // start of line 2
	if line != 1 || col != 0 {
		t.Errorf("off3: %d:%d", line, col)
	}
	line, col = offsetToLineUTF16Col(text, starts, 4) // after 'c'
	if line != 1 || col != 1 {
		t.Errorf("off4: %d:%d want 1:1", line, col)
	}
	emojiEnd := 4 + len("😀")
	line, col = offsetToLineUTF16Col(text, starts, emojiEnd)
	if line != 1 || col != 3 {
		t.Errorf("after emoji: %d:%d want 1:3", line, col)
	}
}

func TestSourceMapIdentityCopy(t *testing.T) {
	m := New()
	m.AddFunc("dummy/copy", func(_ *M, w io.Writer, r io.Reader, _ map[string]string) error {
		return writeReaderBytes(w, r)
	})

	src := []byte("hello\nworld")
	out, ix, err := m.BytesMapped("dummy/copy", src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("copy changed bytes: %q vs %q", out, src)
	}

	raw := ix.SourceMap(src, out, SourceMapOptions{
		File:           "out.txt",
		Source:         "in.txt",
		IncludeContent: true,
	})

	var doc struct {
		Version        int       `json:"version"`
		File           string    `json:"file"`
		Sources        []string  `json:"sources"`
		SourcesContent []*string `json:"sourcesContent"`
		Names          []string  `json:"names"`
		Mappings       string    `json:"mappings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 3 {
		t.Errorf("version %d", doc.Version)
	}
	if doc.File != "out.txt" || len(doc.Sources) != 1 || doc.Sources[0] != "in.txt" {
		t.Errorf("envelope: file=%q sources=%v", doc.File, doc.Sources)
	}
	if doc.SourcesContent == nil || doc.SourcesContent[0] == nil || *doc.SourcesContent[0] != string(src) {
		t.Errorf("sourcesContent missing")
	}
	if len(doc.Names) != 0 {
		t.Errorf("names should be empty: %v", doc.Names)
	}

	points := decodeMappings(doc.Mappings)
	if len(points) == 0 {
		t.Fatalf("no mapping points; mappings=%q", doc.Mappings)
	}
	for _, p := range points {
		if p.genLine != p.origLine || p.genCol != p.origCol {
			t.Errorf("identity map drift: gen %d:%d → orig %d:%d",
				p.genLine, p.genCol, p.origLine, p.origCol)
		}
	}

	for g := 0; g < len(out); g++ {
		loc, exact, err := ix.Original(g)
		if err != nil || !exact {
			t.Fatalf("Original(%d): exact=%v err=%v", g, exact, err)
		}
		if loc.Offset != g {
			t.Fatalf("Original(%d).Offset=%d", g, loc.Offset)
		}
	}
}

func TestSourceMapUsesMinifiedLineStarts(t *testing.T) {
	// Even if incremental genLines were wrong, serialization should use a
	// scan of the minified text when lengths match.
	pristine := []byte("a\r\nb")
	working := append([]byte(nil), pristine...)
	var out bytes.Buffer
	tw := newTrackingWriter(&out, working, pristine)
	if _, err := tw.Write([]byte("a\r")); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("\nb")); err != nil {
		t.Fatal(err)
	}
	ix := tw.finish(len(pristine))
	minified := out.Bytes()

	// Corrupt stored genLines to simulate the old split-CRLF bug.
	ix.genLines = []int{0, 2, 3}
	raw := ix.SourceMap(pristine, minified, SourceMapOptions{})
	var doc struct {
		Mappings string `json:"mappings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	points := decodeMappings(doc.Mappings)
	// With correct lineStarts(minified)=[0,3], content is two lines; 'b' is line 1.
	// A wrong genLines of [0,2,3] would invent an extra empty line.
	for _, p := range points {
		if p.genLine > 1 {
			t.Fatalf("unexpected genLine %d (stale genLines leaked into map); mappings=%q",
				p.genLine, doc.Mappings)
		}
	}
}

func TestSourceMapClaimOnceMultiEmit(t *testing.T) {
	pristine := []byte("function f(name){return name+name}")
	working := make([]byte, len(pristine), len(pristine)+1)
	copy(working, pristine)

	var out bytes.Buffer
	tw := newTrackingWriter(&out, working, pristine)
	if _, err := tw.Write(working); err != nil {
		t.Fatal(err)
	}
	// Re-emit shared "name" slice → non-exact under claim-once.
	if _, err := tw.Write(working[11:15]); err != nil {
		t.Fatal(err)
	}
	ix := tw.finish(len(pristine))
	minified := out.Bytes()

	raw := ix.SourceMap(pristine, minified, SourceMapOptions{
		File:           "f.min.js",
		Source:         "f.js",
		IncludeContent: true,
	})
	if !json.Valid(raw) {
		t.Fatalf("invalid JSON: %s", raw)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["mappings"] == "" {
		t.Fatal("empty mappings")
	}
	a := ix.AuditSegments(pristine, minified)
	if a.GenIssues > 0 || a.ExactOverlaps > 0 {
		t.Fatalf("audit failed:\n%s", a.Report)
	}
}

// TestDumpSourceMapVisualizer writes files for
// https://evanw.github.io/source-map-visualization/
//
//	DUMP_SOURCEMAP=/tmp/sm go test -run TestDumpSourceMapVisualizer -v
//	DUMP_SOURCEMAP=1 go test -run TestDumpSourceMapVisualizer -v   # uses t.TempDir
func TestDumpSourceMapVisualizer(t *testing.T) {
	dir := os.Getenv("DUMP_SOURCEMAP")
	if dir == "" {
		t.Skip("set DUMP_SOURCEMAP=1 or a directory path to write visualizer fixtures")
	}
	if dir == "1" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	src := []byte("" +
		"// demo for source-map-visualization\n" +
		"function greet(name) {\n" +
		"  return 'hi ' + name + '!';\n" +
		"}\n" +
		"greet( 'world' );\n")

	working := make([]byte, len(src), len(src)+1)
	copy(working, src)

	var buf bytes.Buffer
	tw := newTrackingWriter(&buf, working, src)
	write := func(b []byte) {
		t.Helper()
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}

	// Minified shape: function greet(name){return'hi '+name+'!'}greet('world');
	fn := bytes.Index(src, []byte("function"))
	gr := bytes.Index(src, []byte("greet"))
	nameDecl := bytes.Index(src, []byte("(name)")) + 1 // 'n' of parameter

	write(working[fn : fn+8]) // function
	write([]byte(" "))
	write(working[gr : gr+5]) // greet (decl/name first claim)
	write([]byte("("))
	write(working[nameDecl : nameDecl+4]) // name binding
	write([]byte("){return"))
	write([]byte("'hi '"))
	write([]byte("+"))
	write(working[nameDecl : nameDecl+4]) // name use → non-exact (claim-once)
	write([]byte("+"))
	write([]byte("'!'"))
	write([]byte("}"))
	write(working[gr : gr+5]) // greet call → non-exact
	write([]byte("("))
	write([]byte("'world'"))
	write([]byte(");"))

	out := buf.Bytes()
	ix := tw.finish(len(src))
	mapJSON := ix.SourceMap(src, out, SourceMapOptions{
		File:           "demo.min.js",
		Source:         "demo.js",
		IncludeContent: true,
	})

	minWithURL := append(append([]byte{}, out...),
		[]byte("\n//# sourceMappingURL=demo.min.js.map\n")...)

	for _, f := range []struct {
		name string
		data []byte
	}{
		{"demo.js", src},
		{"demo.min.js", minWithURL},
		{"demo.min.js.map", mapJSON},
	} {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", p, len(f.data))
	}

	a := ix.AuditSegments(src, out)
	t.Logf("coverage=%.4f segs=%d overlaps=%d genIssues=%d",
		ix.Coverage(), len(ix.segs), a.ExactOverlaps, a.GenIssues)
	t.Logf("minified: %s", out)
	t.Logf("Open https://evanw.github.io/source-map-visualization/ and load files from %s", dir)
	if a.GenIssues > 0 || a.ExactOverlaps > 0 {
		t.Fatalf("audit:\n%s", a.Report)
	}
}

// --- test-only VLQ / mappings decode ---

func decodeVLQ(b []byte) (int, []byte) {
	var u uint32
	var shift uint
	for len(b) > 0 {
		c := b[0]
		b = b[1:]
		idx := strings.IndexByte(vlqBase64, c)
		if idx < 0 {
			return 0, b
		}
		digit := uint32(idx)
		u |= (digit & 31) << shift
		shift += 5
		if digit&32 == 0 {
			break
		}
	}
	if u&1 != 0 {
		return -int(u >> 1), b
	}
	return int(u >> 1), b
}

type decodedPoint struct {
	genLine, genCol   int
	origLine, origCol int
	sourceIdx         int
}

func decodeMappings(mappings string) []decodedPoint {
	var points []decodedPoint
	genLine := 0
	genCol := 0
	srcIdx := 0
	origLine := 0
	origCol := 0
	b := []byte(mappings)
	for len(b) > 0 {
		if b[0] == ';' {
			genLine++
			genCol = 0
			b = b[1:]
			continue
		}
		if b[0] == ',' {
			b = b[1:]
			continue
		}
		var v int
		v, b = decodeVLQ(b)
		genCol += v
		if len(b) == 0 || b[0] == ';' || b[0] == ',' {
			points = append(points, decodedPoint{genLine: genLine, genCol: genCol})
			continue
		}
		v, b = decodeVLQ(b)
		srcIdx += v
		v, b = decodeVLQ(b)
		origLine += v
		v, b = decodeVLQ(b)
		origCol += v
		if len(b) > 0 && b[0] != ';' && b[0] != ',' {
			_, b = decodeVLQ(b)
		}
		points = append(points, decodedPoint{
			genLine: genLine, genCol: genCol,
			origLine: origLine, origCol: origCol,
			sourceIdx: srcIdx,
		})
	}
	return points
}

