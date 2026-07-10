package minify_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/html"
	"github.com/tdewolff/minify/v2/js"
	minjson "github.com/tdewolff/minify/v2/json"
	"github.com/tdewolff/minify/v2/svg"
	"github.com/tdewolff/minify/v2/xml"
	"github.com/tdewolff/test"
)

func commonM() *minify.M {
	m := minify.New()
	m.AddFunc("text/css", css.Minify)
	m.AddFunc("text/html", html.Minify)
	m.AddFunc("application/javascript", js.Minify)
	m.AddFunc("text/javascript", js.Minify)
	m.AddFunc("application/json", minjson.Minify)
	m.AddFunc("image/svg+xml", svg.Minify)
	m.AddFunc("text/xml", xml.Minify)
	m.AddFunc("application/xml", xml.Minify)
	return m
}

func assertExactInvariant(t *testing.T, m *minify.M, mediatype string, pristine []byte) {
	t.Helper()
	out, ix, err := m.BytesMapped(mediatype, pristine)
	test.Error(t, err)
	test.That(t, ix != nil)

	for g := 0; g < len(out); g++ {
		loc, exact, err := ix.Original(g)
		test.Error(t, err)
		if !exact {
			if loc.Offset < 0 || loc.Offset > len(pristine) {
				t.Fatalf("%s gen=%d non-exact offset %d out of range [0,%d]", mediatype, g, loc.Offset, len(pristine))
			}
			continue
		}
		if loc.Offset < 0 || loc.Offset >= len(pristine) {
			t.Fatalf("%s gen=%d exact offset %d out of range", mediatype, g, loc.Offset)
		}
		if out[g] != pristine[loc.Offset] {
			t.Fatalf("%s gen=%d exact byte mismatch: minified %q vs original[%d]=%q",
				mediatype, g, out[g], loc.Offset, pristine[loc.Offset])
		}
	}

	_, _, err = ix.Original(len(out))
	test.Error(t, err)
	_, _, err = ix.Original(-1)
	test.T(t, err, minify.ErrOutOfRange)
	_, _, err = ix.Original(len(out) + 1)
	test.T(t, err, minify.ErrOutOfRange)

	a := ix.AuditSegments(pristine, out)
	if a.GenIssues > 0 {
		t.Errorf("%s: gen structural issues:\n%s", mediatype, a.Report)
	}
	// Exact overlaps are correctness issues (multi-emit). Surface them.
	if a.ExactOverlaps > 0 {
		t.Errorf("%s: %d exact orig overlaps (%d bytes):\n%s",
			mediatype, a.ExactOverlaps, a.ExactOverlapB, a.Report)
	}
}

func TestExactInvariantJS(t *testing.T) {
	m := commonM()
	samples := []string{
		`var foo = 1;`,
		`function f(a, b) { return a + b; }`,
		`console.log("hi");`,
		`let x=1;x=2;`,
	}
	for _, s := range samples {
		assertExactInvariant(t, m, "application/javascript", []byte(s))
	}
}

func TestExactInvariantCSS(t *testing.T) {
	m := commonM()
	samples := []string{
		`body { color: red; }`,
		`.foo { margin: 0; padding: 10px; }`,
		`@media screen { a { display: none; } }`,
	}
	for _, s := range samples {
		assertExactInvariant(t, m, "text/css", []byte(s))
	}
}

func TestExactInvariantHTML(t *testing.T) {
	m := commonM()
	samples := []string{
		`<!doctype html><html><head><title>x</title></head><body><p>hi</p></body></html>`,
		`<div class="a"><span>text</span></div>`,
		`<script>var x=1;</script><style>a{color:red}</style>`,
	}
	for _, s := range samples {
		assertExactInvariant(t, m, "text/html", []byte(s))
	}
}

func TestCoverageFloors(t *testing.T) {
	m := commonM()
	cases := []struct {
		mediatype string
		src       string
		floor     float64
	}{
		// Floors sit below measured values with headroom for minifier churn.
		{"application/javascript", `function hello(name){return "hi "+name;}`, 0.25},
		{"text/css", `body{color:red;margin:0;padding:10px}`, 0.25},
		{"text/html", `<!doctype html><html><body><p class="x">Hello</p><script>var a=1;</script></body></html>`, 0.35},
	}
	for _, tc := range cases {
		_, ix, err := m.BytesMapped(tc.mediatype, []byte(tc.src))
		test.Error(t, err)
		cov := ix.Coverage()
		if cov < tc.floor {
			t.Fatalf("%s coverage %.3f below floor %.3f", tc.mediatype, cov, tc.floor)
		}
		t.Logf("%s coverage=%.3f", tc.mediatype, cov)
	}
}

func TestTemplateExactMapping(t *testing.T) {
	m := minify.New()
	m.AddFunc("text/html", (&html.Minifier{TemplateDelims: html.GoTemplateDelims}).Minify)
	m.AddFunc("text/css", css.Minify)
	m.AddFunc("application/javascript", js.Minify)

	const action = "{{.User.Name}}"
	src := []byte(`<!doctype html><html><body><p>Hello ` + action + `!</p></body></html>`)
	origActionOff := bytes.Index(src, []byte(action))
	test.That(t, origActionOff >= 0)

	out, ix, err := m.BytesMapped("text/html", src)
	test.Error(t, err)

	genActionOff := bytes.Index(out, []byte(action))
	if genActionOff < 0 {
		t.Fatalf("template action missing from minified output: %q", out)
	}

	for i := 0; i < len(action); i++ {
		loc, exact, err := ix.Original(genActionOff + i)
		test.Error(t, err)
		if !exact {
			t.Fatalf("expected exact mapping for template byte %d, got loc=%+v", i, loc)
		}
		want := origActionOff + i
		if loc.Offset != want {
			t.Fatalf("template byte %d: got offset %d want %d", i, loc.Offset, want)
		}
	}
}

func TestOriginalForPosition(t *testing.T) {
	m := commonM()
	src := []byte("var a = 1;\nvar b = 2;\n")
	out, ix, err := m.BytesMapped("application/javascript", src)
	test.Error(t, err)
	test.That(t, len(out) > 0)

	loc, _, err := ix.OriginalForPosition(1, 1)
	test.Error(t, err)
	test.That(t, loc.Line >= 1)
	test.That(t, loc.LinePos >= 1)

	_, _, err = ix.OriginalForPosition(0, 1)
	test.T(t, err, minify.ErrOutOfRange)
}

func TestMinifyMappedJSMatchesBytes(t *testing.T) {
	m := commonM()
	src := []byte(`function f(){ return 1; }`)
	var buf bytes.Buffer
	ix, err := m.MinifyMapped("application/javascript", &buf, src)
	test.Error(t, err)
	test.That(t, ix != nil)

	out2, err := m.Bytes("application/javascript", append([]byte(nil), src...))
	test.Error(t, err)
	test.Bytes(t, buf.Bytes(), out2)
}

func TestSourceMapJSRealMinify(t *testing.T) {
	m := commonM()
	src := []byte("" +
		"function greet(name) {\n" +
		"  return 'hi ' + name + name;\n" +
		"}\n" +
		"greet('world');\n")
	out, ix, err := m.BytesMapped("application/javascript", src)
	test.Error(t, err)

	raw := ix.SourceMap(src, out, minify.SourceMapOptions{
		File:           "greet.min.js",
		Source:         "greet.js",
		IncludeContent: true,
	})
	if !json.Valid(raw) {
		t.Fatalf("invalid source map JSON: %s", raw)
	}
	var doc struct {
		Version  int      `json:"version"`
		Sources  []string `json:"sources"`
		Names    []string `json:"names"`
		Mappings string   `json:"mappings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 3 || doc.Mappings == "" || len(doc.Sources) != 1 {
		t.Fatalf("bad envelope: %+v", doc)
	}
	if len(doc.Names) != 0 {
		t.Fatalf("names should be empty until Phase 4: %v", doc.Names)
	}

	a := ix.AuditSegments(src, out)
	if a.GenIssues > 0 || a.ExactOverlaps > 0 {
		t.Fatalf("audit:\n%s", a.Report)
	}
	t.Logf("out=%q coverage=%.4f", out, ix.Coverage())
}

// TestDumpSourceMapVisualizerReal writes real-minifier fixtures for
// https://evanw.github.io/source-map-visualization/
//
//	DUMP_SOURCEMAP=/tmp/sm go test -run TestDumpSourceMapVisualizerReal -v
func TestDumpSourceMapVisualizerReal(t *testing.T) {
	dir := os.Getenv("DUMP_SOURCEMAP")
	if dir == "" {
		t.Skip("set DUMP_SOURCEMAP to a directory (or 1 for TempDir)")
	}
	if dir == "1" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := commonM()
	// Keep var names so multi-emit "name" is visible in the visualizer.
	m.AddFunc("application/javascript", (&js.Minifier{KeepVarNames: true}).Minify)

	cases := []struct {
		name, mediatype string
		src             []byte
	}{
		{
			name:      "greet",
			mediatype: "application/javascript",
			src: []byte("" +
				"// demo for source-map-visualization\n" +
				"function greet(name) {\n" +
				"  return 'hi ' + name + name;\n" +
				"}\n" +
				"greet('world');\n"),
		},
		{
			name:      "page",
			mediatype: "text/html",
			src: []byte("" +
				"<!doctype html>\n" +
				"<html>\n" +
				"<head>\n" +
				"  <style>\n" +
				"    .box { color: #ff0000; margin: 0px; }\n" +
				"  </style>\n" +
				"</head>\n" +
				"<body>\n" +
				"  <h1 class=\"box\">Hello</h1>\n" +
				"  <script>\n" +
				"    function greet(name) {\n" +
				"      return 'hi ' + name;\n" +
				"    }\n" +
				"    greet('world');\n" +
				"  </script>\n" +
				"</body>\n" +
				"</html>\n"),
		},
	}

	for _, tc := range cases {
		out, ix, err := m.BytesMapped(tc.mediatype, tc.src)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		ext := filepath.Ext(tc.name)
		if ext == "" {
			switch tc.mediatype {
			case "application/javascript":
				ext = ".js"
			case "text/html":
				ext = ".html"
			default:
				ext = ".txt"
			}
		}
		base := tc.name
		srcName := base + ext
		minName := base + ".min" + ext
		mapName := minName + ".map"

		mapJSON := ix.SourceMap(tc.src, out, minify.SourceMapOptions{
			File:           minName,
			Source:         srcName,
			IncludeContent: true,
		})
		// Write minified bytes only (no sourceMappingURL trailer) so line/column
		// counts match the map in visualizers that paste the three artifacts.

		for _, f := range []struct {
			name string
			data []byte
		}{
			{srcName, tc.src},
			{minName, out},
			{mapName, mapJSON},
		} {
			p := filepath.Join(dir, f.name)
			if err := os.WriteFile(p, f.data, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s (%d bytes)", p, len(f.data))
		}
		a := ix.AuditSegments(tc.src, out)
		t.Logf("%s coverage=%.4f overlaps=%d genIssues=%d out=%q",
			tc.name, ix.Coverage(), a.ExactOverlaps, a.GenIssues, out)
		if a.GenIssues > 0 || a.ExactOverlaps > 0 {
			t.Errorf("%s audit:\n%s", tc.name, a.Report)
		}
	}
	t.Logf("Open https://evanw.github.io/source-map-visualization/ and load from %s", dir)
}

// --- benchmarks corpus ---

func mediatypeForSample(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".js":
		return "application/javascript"
	case ".css":
		return "text/css"
	case ".html", ".htm":
		return "text/html"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".xml":
		return "text/xml"
	default:
		return ""
	}
}

// TestBenchmarksMappingMetrics minifies every _benchmarks sample with mapping,
// checks generated-space structure, and reports exact-orig overlaps (multi-emit).
//
// Exact overlaps are treated as failures: coverage that counts multi-emitted
// exact slices is not trustworthy for occurrence identity.
func TestBenchmarksMappingMetrics(t *testing.T) {
	const dir = "_benchmarks"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	m := commonM()

	type row struct {
		name             string
		in, out          int
		cov              float64
		segs             int
		exactB, nonExact int
		genIssues        int
		exactOverlap     int
		exactOverlapB    int
		exactOOO         int
	}
	var rows []row
	var totalOverlap, totalOverlapB, filesWithOverlap int

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "sample_") {
			continue
		}
		mt := mediatypeForSample(e.Name())
		if mt == "" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", path, err)
			continue
		}

		// Capture loop vars for subtest.
		name, mediatype := e.Name(), mt
		srcCopy := src

		t.Run(name, func(t *testing.T) {
			out, ix, err := m.BytesMapped(mediatype, srcCopy)
			if err != nil {
				t.Fatalf("BytesMapped: %v", err)
			}
			a := ix.AuditSegments(srcCopy, out)

			r := row{
				name:          name,
				in:            len(srcCopy),
				out:           len(out),
				cov:           ix.Coverage(),
				segs:          a.SegCount,
				exactB:        a.ExactBytes,
				nonExact:      a.NonExactBytes,
				genIssues:     a.GenIssues,
				exactOverlap:  a.ExactOverlaps,
				exactOverlapB: a.ExactOverlapB,
				exactOOO:      a.ExactOutOfOrder,
			}
			rows = append(rows, r)

			if a.GenIssues > 0 || a.ExactOverlaps > 0 {
				t.Log("\n" + a.Report)
			} else {
				t.Logf("ok in=%d out=%d cov=%.4f segs=%d exactB=%d outOfOrder=%d",
					r.in, r.out, r.cov, r.segs, r.exactB, r.exactOOO)
			}

			if a.GenIssues > 0 {
				t.Errorf("generated-space structural issues: %d", a.GenIssues)
			}
			if a.ExactOverlaps > 0 {
				totalOverlap += a.ExactOverlaps
				totalOverlapB += a.ExactOverlapB
				filesWithOverlap++
				t.Errorf("exact orig overlaps: %d segments, %d generated bytes (multi-emit)",
					a.ExactOverlaps, a.ExactOverlapB)
			}

			// Necessary but not sufficient: exact bytes match pristine.
			for g := 0; g < len(out); g++ {
				loc, exact, err := ix.Original(g)
				if err != nil {
					t.Fatalf("Original(%d): %v", g, err)
				}
				if exact {
					if loc.Offset < 0 || loc.Offset >= len(srcCopy) || out[g] != srcCopy[loc.Offset] {
						t.Fatalf("exact byte mismatch g=%d loc=%+v", g, loc)
					}
				}
			}
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n%-32s %10s %10s %8s %8s %10s %10s %8s %8s %8s\n",
		"file", "in", "out", "cov", "segs", "exactB", "overlap", "ovlB", "ooo", "genIss")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-32s %10d %10d %8.4f %8d %10d %10d %8d %8d %8d\n",
			r.name, r.in, r.out, r.cov, r.segs, r.exactB, r.exactOverlap, r.exactOverlapB, r.exactOOO, r.genIssues)
	}
	fmt.Fprintf(&b, "TOTAL files=%d withExactOverlap=%d exactOverlaps=%d exactOverlapBytes=%d\n",
		len(rows), filesWithOverlap, totalOverlap, totalOverlapB)
	t.Log(b.String())
}

// TestJSVarMultiEmitAudit checks claim-once for shared Var.Data re-emits.
// First claim is exact at the binding; later uses are non-exact to that binding
// (no fuzzy forward search for other occurrences of the same bytes).
func TestJSVarMultiEmitAudit(t *testing.T) {
	src := []byte("function f(name){return name+name}")
	needle := []byte("name")
	bindingOrig := bytes.Index(src, needle)
	if bindingOrig < 0 {
		t.Fatal("binding not found in source")
	}

	m := minify.New()
	m.AddFunc("application/javascript", (&js.Minifier{KeepVarNames: true}).Minify)
	out, ix, err := m.BytesMapped("application/javascript", src)
	if err != nil {
		t.Fatal(err)
	}
	a := ix.AuditSegments(src, out)
	t.Log("\n" + a.Report)
	if a.GenIssues > 0 {
		t.Errorf("gen issues: %d", a.GenIssues)
	}
	if a.ExactOverlaps > 0 {
		t.Errorf("exact overlaps after claim-once policy: %d (%d bytes)", a.ExactOverlaps, a.ExactOverlapB)
	}

	// Locate "name" in minified output rather than hard-coding gen offsets.
	var nameGens []int
	for i := 0; i+len(needle) <= len(out); i++ {
		if bytes.Equal(out[i:i+len(needle)], needle) {
			nameGens = append(nameGens, i)
		}
	}
	if len(nameGens) < 3 {
		t.Fatalf("expected at least 3 %q in output %q, found %d at %v", needle, out, len(nameGens), nameGens)
	}

	// First occurrence is the binding (exact); later uses are non-exact to binding.
	loc, exact, err := ix.Original(nameGens[0])
	if err != nil {
		t.Fatal(err)
	}
	if !exact || loc.Offset != bindingOrig {
		t.Errorf("binding: exact=%v orig=%d want exact@%d", exact, loc.Offset, bindingOrig)
	}
	for _, genAt := range nameGens[1:] {
		loc, exact, err := ix.Original(genAt)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("gen[%d]=%q → orig=%d exact=%v", genAt, out[genAt:genAt+1], loc.Offset, exact)
		if exact {
			t.Errorf("use at gen %d should not be exact (multi-emit of binding slice)", genAt)
		}
		if loc.Offset != bindingOrig {
			t.Errorf("use at gen %d: orig=%d want %d (binding)", genAt, loc.Offset, bindingOrig)
		}
	}
}
