package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

const (
	// The host cancels a parse after 5 s, and a cancelled file keeps its stale
	// chunks. A deadline inside the plugin is not an option: the host does not
	// enable wazero's real clocks, so time.Now() is fake there. Big files are
	// therefore not parsed; they become plain `file` chunks.
	maxParseSize    = 80 << 10
	maxFallbackSize = 2 << 20 // above this the file is skipped (generated/vendored)
	maxChunkSize    = 8 << 10 // longer function bodies are split at line boundaries
)

// Chunk is the universal contract defined in the Onfly SDD.
type Chunk struct {
	FilePath string            `json:"file_path"`
	Type     string            `json:"type"`
	Content  string            `json:"content"`
	Metadata map[string]string `json:"metadata"`
}

// fatalError writes to stderr so the Onfly core can log it without
// breaking the stdout JSON pipeline.
func fatalError(err error) {
	fmt.Fprintf(os.Stderr, "onfly-python-parser error: %v\n", err)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatalError(fmt.Errorf("missing file path argument"))
	}
	filePath := os.Args[1]
	src, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatalError(fmt.Errorf("failed to read stdin: %w", err))
	}
	chunks, err := parse(filePath, src)
	if err != nil {
		fatalError(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(chunks); err != nil {
		fatalError(fmt.Errorf("failed to encode json: %w", err))
	}
}

type walker struct {
	src    []byte
	lang   *gotreesitter.Language
	file   string
	lines  []int // byte offset of each line start
	chunks []Chunk
}

func parse(filePath string, src []byte) (chunks []Chunk, err error) {
	defer func() {
		if r := recover(); r != nil {
			chunks, err = nil, fmt.Errorf("parser panic: %v", r)
		}
	}()
	// Initialized empty so JSON serializes as [] instead of null.
	chunks = []Chunk{}
	if len(src) == 0 {
		return chunks, nil
	}
	w := &walker{src: src, file: filePath, lines: []int{0}, chunks: chunks}
	for i, b := range src {
		if b == '\n' {
			w.lines = append(w.lines, i+1)
		}
	}
	switch {
	case len(src) > maxFallbackSize:
		fmt.Fprintf(os.Stderr, "onfly-python-parser: %s is %d bytes, skipped\n", filePath, len(src))
		return chunks, nil
	case len(src) > maxParseSize:
		return w.wholeFile("too_large"), nil
	}
	w.lang = grammars.PythonLanguage()
	tree, err := gotreesitter.NewParser(w.lang).Parse(src)
	if err != nil {
		return nil, fmt.Errorf("failed to parse python file: %w", err)
	}
	root := tree.RootNode()
	if root.HasError() {
		fmt.Fprintf(os.Stderr, "onfly-python-parser: %s has syntax errors, chunking what parsed\n", filePath)
	}
	w.block(root, "")
	if len(w.chunks) == 0 {
		return w.wholeFile(""), nil
	}
	return w.chunks, nil
}

// wholeFile returns the file as `file` chunks (split by emit at line boundaries).
func (w *walker) wholeFile(reason string) []Chunk {
	meta := map[string]string{"name": w.file}
	if reason != "" {
		meta["reason"] = reason
	}
	w.emit("file", 0, uint32(len(w.src)), meta)
	return w.chunks
}

// ---- helpers ----

func (w *walker) typ(n *gotreesitter.Node) string { return n.Type(w.lang) }
func (w *walker) text(n *gotreesitter.Node) string {
	if n == nil {
		return ""
	}
	return string(w.src[n.StartByte():n.EndByte()])
}
func (w *walker) field(n *gotreesitter.Node, name string) *gotreesitter.Node {
	return n.ChildByFieldName(name, w.lang)
}

func (w *walker) contains(n *gotreesitter.Node, types ...string) bool {
	t := w.typ(n)
	for _, want := range types {
		if t == want {
			return true
		}
	}
	for i := 0; i < n.ChildCount(); i++ {
		if w.contains(n.Child(i), types...) {
			return true
		}
	}
	return false
}

// docStart widens a node's start to include `#` comments directly above it
// (no blank line between, and each comment on a line of its own).
func (w *walker) docStart(n *gotreesitter.Node) uint32 {
	start := n.StartByte()
	for p := n.PrevSibling(); p != nil && w.typ(p) == "comment"; p = p.PrevSibling() {
		if strings.Count(string(w.src[p.EndByte():start]), "\n") > 1 {
			break
		}
		ls := w.lines[w.line(p.StartByte())-1]
		if strings.TrimSpace(string(w.src[ls:p.StartByte()])) != "" {
			break
		}
		start = p.StartByte()
	}
	return start
}

func (w *walker) line(off uint32) int {
	return sort.Search(len(w.lines), func(i int) bool { return w.lines[i] > int(off) })
}

func (w *walker) emit(typ string, start, end uint32, meta map[string]string) {
	for end > start && strings.TrimSpace(string(w.src[end-1:end])) == "" {
		end--
	}
	if end <= start {
		return
	}
	meta["start_line"] = strconv.Itoa(w.line(start))
	meta["end_line"] = strconv.Itoa(w.line(end - 1))
	content := string(w.src[start:end])
	if len(content) <= maxChunkSize || typ == "class" {
		w.chunks = append(w.chunks, Chunk{FilePath: w.file, Type: typ, Content: content, Metadata: meta})
		return
	}
	var parts []string
	for len(content) > maxChunkSize {
		cut := strings.LastIndex(content[:maxChunkSize], "\n")
		if cut <= 0 {
			cut = maxChunkSize
			for cut > 0 && !utf8.RuneStart(content[cut]) {
				cut--
			}
		}
		parts = append(parts, content[:cut])
		content = content[cut:]
	}
	parts = append(parts, content)
	for i, p := range parts {
		m := make(map[string]string, len(meta)+1)
		for k, v := range meta {
			m[k] = v
		}
		m["part"] = fmt.Sprintf("%d/%d", i+1, len(parts))
		w.chunks = append(w.chunks, Chunk{FilePath: w.file, Type: typ, Content: p, Metadata: m})
	}
}

func join(scope, name string) string {
	if scope == "" {
		return name
	}
	return scope + "." + name
}

// ---- walk ----

var declTypes = []string{"function_definition", "class_definition", "decorated_definition", "import_statement", "import_from_statement", "future_import_statement"}

// containers hold nested statements; we only descend into them when they declare
// something (`if TYPE_CHECKING: import x`, `try: import y / except ImportError: ...`).
var containers = map[string]bool{
	"if_statement": true, "elif_clause": true, "else_clause": true, "try_statement": true,
	"except_clause": true, "except_group_clause": true, "finally_clause": true, "with_statement": true,
}

// block walks the statements of the module, a class body, or a container
// statement that declares something. receiver is the owning class's dotted
// name, empty at module level.
func (w *walker) block(parent *gotreesitter.Node, receiver string) {
	inClass := receiver != "" && w.typ(parent) == "block"
	nested := containers[w.typ(parent)]
	var run []*gotreesitter.Node
	flush := func() {
		if len(run) == 0 {
			return
		}
		first, last := run[0], run[len(run)-1]
		w.emit("statement", w.docStart(first), last.EndByte(), map[string]string{"name": "statements"})
		run = nil
	}
	for i := 0; i < parent.ChildCount(); i++ {
		c := parent.Child(i)
		switch t := w.typ(c); t {
		case "comment":
		case "import_statement", "import_from_statement", "future_import_statement":
			flush()
			w.emit("import", c.StartByte(), c.EndByte(), map[string]string{"name": strings.Join(strings.Fields(w.text(c)), " ")})
		case "function_definition", "class_definition", "decorated_definition":
			flush()
			w.definition(c, c, receiver)
		case "type_alias_statement":
			flush()
			m := map[string]string{"name": w.text(c.Child(1))}
			if inClass {
				m["receiver"] = receiver
			}
			w.emit("type", w.docStart(c), c.EndByte(), m)
		case "block":
			w.block(c, receiver)
		case "assignment":
			if l := w.field(c, "left"); l != nil && w.typ(l) == "identifier" {
				flush()
				w.variable(c, receiver)
				continue
			}
			if !inClass && !nested {
				run = append(run, c)
			}
		default:
			if containers[t] && w.contains(c, declTypes...) {
				flush()
				w.block(c, receiver)
				continue
			}
			if !nested && !inClass {
				run = append(run, c)
			}
		}
	}
	flush()
}

func (w *walker) variable(a *gotreesitter.Node, receiver string) {
	name := w.text(w.field(a, "left"))
	m := map[string]string{"name": name}
	if t := w.field(a, "type"); t != nil {
		m["type"] = w.text(t)
	}
	typ := "variable"
	switch {
	case receiver != "":
		typ = "property"
		m["receiver"] = receiver
		m["fqn"] = receiver + "." + name
	case strings.ToUpper(name) == name && strings.ContainsAny(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZ"):
		typ = "constant"
		m["fqn"] = name
	default:
		m["fqn"] = name
	}
	w.emit(typ, w.docStart(a), a.EndByte(), m)
}

// definition emits a function, method or class. outer is the node whose range
// is chunked (the decorated_definition when there are decorators), n the def itself.
func (w *walker) definition(outer, n *gotreesitter.Node, receiver string) {
	var decorators []string
	if w.typ(n) == "decorated_definition" {
		for i := 0; i < n.ChildCount(); i++ {
			if c := n.Child(i); w.typ(c) == "decorator" {
				decorators = append(decorators, w.text(c))
			}
		}
		n = w.field(n, "definition")
		if n == nil {
			return
		}
	}
	name := w.text(w.field(n, "name"))
	fqn := join(receiver, name)
	m := map[string]string{"name": name, "fqn": fqn}
	if receiver != "" {
		m["receiver"] = receiver
	}
	if len(decorators) > 0 {
		m["decorators"] = strings.Join(decorators, " ")
	}
	body := w.field(n, "body")
	// The signature ends at the ':' before the body (a trailing comment may follow it).
	sigEnd := n.EndByte()
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); w.typ(c) == ":" && (body == nil || c.EndByte() <= body.StartByte()) {
			sigEnd = c.EndByte()
		}
	}
	switch w.typ(n) {
	case "function_definition":
		typ := "function"
		if receiver != "" {
			typ = "method"
		}
		if n.ChildCount() > 0 && w.typ(n.Child(0)) == "async" {
			m["modifiers"] = "async"
		}
		m["signature"] = strings.Join(strings.Fields(strings.TrimSuffix(string(w.src[n.StartByte():sigEnd]), ":")), " ")
		if p := w.field(n, "parameters"); p != nil {
			m["params"] = w.text(p)
		}
		if r := w.field(n, "return_type"); r != nil {
			m["return_type"] = w.text(r)
		}
		w.emit(typ, w.docStart(outer), outer.EndByte(), m)
	case "class_definition":
		if s := w.field(n, "superclasses"); s != nil {
			m["extends"] = strings.TrimSuffix(strings.TrimPrefix(w.text(s), "("), ")")
		}
		// Header only (comments, decorators, signature, docstring): members are
		// separate chunks that point back through `receiver`.
		end := sigEnd
		if body != nil && body.ChildCount() > 0 {
			if d := body.Child(0); w.typ(d) == "string" {
				end = d.EndByte()
			}
		}
		w.emit("class", w.docStart(outer), end, m)
		if body != nil {
			w.block(body, fqn)
		}
	}
}
