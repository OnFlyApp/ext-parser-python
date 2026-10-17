package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func kinds(t *testing.T, src string) map[string]Chunk {
	t.Helper()
	chunks, err := parse("src/x.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Chunk{}
	for _, c := range chunks {
		key := c.Type + " " + c.Metadata["fqn"]
		if c.Metadata["fqn"] == "" {
			key = c.Type + " " + c.Metadata["name"]
		}
		out[key] = c
	}
	return out
}

func keys(m map[string]Chunk) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	return k
}

func TestAllChunkTypes(t *testing.T) {
	src, err := os.ReadFile("testdata/all.py")
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(t, string(src))
	for _, want := range []string{
		"import import os, sys", "constant MAX_RETRIES", "variable default_name", "type Alias",
		"class Service", "property Service.TIMEOUT", "property Service.name", "class Service.Inner",
		"method Service.Inner.ping", "method Service.__init__", "method Service.make", "method Service.fetch",
		"function helper", "function main", "statement statements",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing chunk %q; have %v", want, keys(got))
		}
	}
	if _, ok := got["function helper"]; !ok {
		t.Fatal("no top-level helper")
	}
	if _, ok := got["function Service.fetch.helper"]; ok {
		t.Error("nested function must stay inside its parent chunk")
	}
	c := got["class Service"]
	if !strings.HasPrefix(c.Content, "# Describes the service.\n@dataclass") || strings.Contains(c.Content, "__init__") ||
		!strings.Contains(c.Content, "Service docstring") || c.Metadata["extends"] != "Base, metaclass=Meta" || c.Metadata["decorators"] != "@dataclass(frozen=True)" {
		t.Errorf("class header chunk wrong: %+v", c)
	}
	m := got["method Service.fetch"]
	if m.Metadata["receiver"] != "Service" || m.Metadata["modifiers"] != "async" || m.Metadata["return_type"] != "bytes" ||
		m.Metadata["signature"] != "async def fetch(self, url: str) -> bytes" || !strings.Contains(m.Content, "def helper") {
		t.Errorf("method chunk wrong: %+v", m)
	}
}

func TestConditionalDeclarations(t *testing.T) {
	src, err := os.ReadFile("testdata/conditional.py")
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(t, string(src))
	for _, want := range []string{"import import ujson as json", "import import json", "import from foo import Bar", "class Proto", "method Proto.run", "function shim", "function after"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; have %v", want, keys(got))
		}
	}
	if strings.Contains(got["function after"].Content, "not a doc comment") {
		t.Errorf("trailing comment of the previous line leaked into the next declaration")
	}
}

func TestEmptyAndCommentOnly(t *testing.T) {
	if chunks, _ := parse("a.py", nil); chunks == nil || len(chunks) != 0 {
		t.Fatalf("empty file must be [], got %#v", chunks)
	}
	got := kinds(t, "# just a comment\n")
	if _, ok := got["file src/x.py"]; len(got) != 1 || !ok {
		t.Fatalf("comment-only file must yield one file chunk: %v", keys(got))
	}
}

func TestSyntaxErrorKeepsParsedDeclarations(t *testing.T) {
	got := kinds(t, "def ok():\n    pass\n\nclass (:\n")
	if _, ok := got["function ok"]; !ok {
		t.Fatalf("lost valid function: %v", keys(got))
	}
}

func TestLongFunctionIsSplit(t *testing.T) {
	body := strings.Repeat("    x = 1\n", 2000)
	chunks, err := parse("a.py", []byte("def big():\n"+body))
	if err != nil || len(chunks) < 2 || chunks[0].Metadata["part"] == "" {
		t.Fatalf("expected split parts, got %d chunks, err %v", len(chunks), err)
	}
}

func TestOversizedFileBecomesPlainText(t *testing.T) {
	chunks, err := parse("big.py", []byte(strings.Repeat("# padding padding padding\n", maxParseSize/20)))
	if err != nil || len(chunks) < 2 {
		t.Fatalf("want split file chunks, got %d, err %v", len(chunks), err)
	}
	for _, c := range chunks {
		if c.Type != "file" || c.Metadata["reason"] != "too_large" {
			t.Fatalf("unexpected chunk %s %v", c.Type, c.Metadata)
		}
	}
}

func TestHugeFileIsSkipped(t *testing.T) {
	chunks, err := parse("huge.py", []byte(strings.Repeat("x", maxFallbackSize+1)))
	if err != nil || chunks == nil || len(chunks) != 0 {
		t.Fatalf("want [], got %d chunks, err %v", len(chunks), err)
	}
}

func TestSplitKeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("s = 'ёёёёёёёёёёёёёёёёёёёё'; ", 600) // one long line: forces the no-newline fallback
	chunks, err := parse("a.py", []byte("def u():\n    "+body+"\n"))
	if err != nil || len(chunks) < 2 {
		t.Fatalf("expected split, got %d, err %v", len(chunks), err)
	}
	for _, c := range chunks {
		if !utf8.ValidString(c.Content) {
			t.Fatalf("invalid utf-8 in part %s", c.Metadata["part"])
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("class A:\n    def f(self): pass\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		// Syntax garbage is fine; a recovered panic is not.
		if _, err := parse("f.py", b); err != nil && strings.Contains(err.Error(), "panic") {
			t.Fatal(err)
		}
	})
}

// Golden files pin content and metadata, not just chunk presence. Regenerate
// with: go test -tags "grammar_subset grammar_subset_python" -run Golden -update
var update = flag.Bool("update", false, "rewrite golden files")

func TestGolden(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.py")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		chunks, err := parse("src/"+filepath.Base(f), src)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := json.MarshalIndent(chunks, "", "  ")
		golden := strings.TrimSuffix(f, filepath.Ext(f)) + ".golden.json"
		if *update {
			if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%v (run with -update)", err)
		}
		if strings.TrimSpace(string(want)) != string(got) {
			t.Errorf("%s differs from %s (run with -update if intended)", f, golden)
		}
	}
}
