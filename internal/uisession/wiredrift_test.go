package uisession

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The browser cannot import Go, so the event vocabulary is written down twice:
// once as the Kind constants below, and once as the EventKind object in
// internal/web/_ui/src/lib/wire.ts. Nothing in either build catches a rename on
// one side - internal/web carries the bytes without decoding them, on purpose -
// and the way it would show up is a page drawing a conversation that did not
// happen, or quietly drawing nothing where a kind used to be.
//
// This is the thing that catches it. It reads both files as text, because that
// is the only representation the two languages share.

const browserWire = "../web/_ui/src/lib/wire.ts"

func TestTheBrowsersEventVocabularyMatchesThisPackages(t *testing.T) {
	got := browserEventKinds(t)
	want := goEventKinds(t)

	for _, kind := range want {
		if !contains(got, kind) {
			t.Errorf("the browser does not know the event kind %q; add it to EventKind in %s",
				kind, browserWire)
		}
	}
	for _, kind := range got {
		if !contains(want, kind) {
			t.Errorf("the browser knows the event kind %q, which this package does not emit; "+
				"remove it from EventKind in %s", kind, browserWire)
		}
	}
}

// The client-error frame is named on both sides too, and it is the one name a
// front-end must not confuse with an event: "error" is a real kind in a
// conversation, and drawing a refused operation as one puts a failure into the
// timeline at the moment the design says there must not be one.
func TestTheBrowserSpellsTheClientErrorFrameTheWayTheDaemonDoes(t *testing.T) {
	source := readBrowserWire(t)
	const want = `export const CLIENT_ERROR = 'client_error'`
	if !strings.Contains(source, want) {
		t.Fatalf("%s does not declare %s", browserWire, want)
	}
	if contains(browserEventKinds(t), "client_error") {
		t.Error("client_error is listed as an event kind in the browser's vocabulary; " +
			"it is a transport frame, not something that happened in the conversation")
	}
}

// goEventKinds reads the Kind constants out of this package's own source. They
// are untyped-looking string constants in a single block, so the declaration is
// the only place they exist and reflection cannot enumerate them.
func goEventKinds(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "event.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing event.go: %v", err)
	}
	var kinds []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Only the block whose constants are declared as Kind. A future
			// const block in this file must not silently join the vocabulary.
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "Kind" {
				continue
			}
			for _, v := range value.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquoting %s: %v", lit.Value, err)
				}
				kinds = append(kinds, s)
			}
		}
	}
	if len(kinds) < 10 {
		t.Fatalf("found only %d Kind constants in event.go, which cannot be right", len(kinds))
	}
	sort.Strings(kinds)
	return kinds
}

var browserKindLine = regexp.MustCompile(`^\s*[A-Za-z]+:\s*'([a-z_]+)',\s*$`)

// browserEventKinds reads the values out of the `export const EventKind = {...}
// as const` object. It is parsed by shape rather than executed, so this test
// needs no node and runs in the ordinary `go test ./...`.
func browserEventKinds(t *testing.T) []string {
	t.Helper()
	source := readBrowserWire(t)
	const open = "export const EventKind = {"
	start := strings.Index(source, open)
	if start < 0 {
		t.Fatalf("%s does not declare %s", browserWire, open)
	}
	rest := source[start+len(open):]
	end := strings.Index(rest, "} as const")
	if end < 0 {
		t.Fatalf("%s: the EventKind object is not closed with `} as const`", browserWire)
	}
	var kinds []string
	for _, line := range strings.Split(rest[:end], "\n") {
		if m := browserKindLine.FindStringSubmatch(line); m != nil {
			kinds = append(kinds, m[1])
		}
	}
	if len(kinds) < 10 {
		t.Fatalf("found only %d entries in the browser's EventKind, which cannot be right",
			len(kinds))
	}
	sort.Strings(kinds)
	return kinds
}

// The kinds are only half of the vocabulary. An event's *fields* are the other
// half, and renaming a JSON tag is exactly the failure this file exists to
// catch: the browser goes on reading `ctx`, `approval` or `from`, finds
// nothing, and draws a conversation with no context window, no approval dialog
// and a desync it cannot recover from - with every test on both sides green.
func TestTheBrowserKnowsThisPackagesEventFields(t *testing.T) {
	got := browserEventFields(t)
	want := goJSONTags(t, "Event")

	for _, field := range want {
		if !contains(got, field) {
			t.Errorf("the browser's RunEvent has no %q; add it to %s", field, browserWire)
		}
	}
	for _, field := range got {
		if !contains(want, field) {
			t.Errorf("the browser's RunEvent reads %q, which uisession.Event does not write; "+
				"remove it from %s", field, browserWire)
		}
	}
}

// The approval a person answers, the answer they give, and who is attending.
// All three cross the wire inside an event and are read by name in the browser.
//
// Each pair is compared inside its own type and in both directions. Searching
// the whole file instead would pass on any field whose name happens to appear
// somewhere else in it - `name`, `text` and `id` occur in nearly every type -
// and one direction would let the browser read a field the daemon never writes.
func TestTheBrowserKnowsTheApprovalFields(t *testing.T) {
	for _, pair := range []struct{ goType, tsType string }{
		{"Approval", "Approval"},
		{"Option", "ApprovalOption"},
		{"Client", "PresenceClient"},
	} {
		want := goJSONTags(t, pair.goType)
		got := browserFields(t, pair.tsType)
		for _, field := range want {
			if !contains(got, field) {
				t.Errorf("the browser's %s has no %q; add it to %s", pair.tsType, field, browserWire)
			}
		}
		for _, field := range got {
			if !contains(want, field) {
				t.Errorf("the browser's %s reads %q, which uisession.%s does not write; "+
					"remove it from %s", pair.tsType, field, pair.goType, browserWire)
			}
		}
	}
}

// goJSONTags reads the json tag names off one struct in this package. The two
// files it looks in are the two that declare what crosses the wire; a struct
// declared elsewhere would be found by name and reported as having no tags.
func goJSONTags(t *testing.T, name string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var out []string
	for _, path := range []string{"event.go", "approval.go"} {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name.Name != name {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, f := range st.Fields.List {
				if f.Tag == nil {
					continue
				}
				tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`")).Get("json")
				field, _, _ := strings.Cut(tag, ",")
				if field != "" && field != "-" {
					out = append(out, field)
				}
			}
			return false
		})
	}
	if len(out) == 0 {
		t.Fatalf("found no json tags on %s", name)
	}
	sort.Strings(out)
	return out
}

// browserFieldLine matches one property declaration. The optional quotes are
// what stops a browser-only field being smuggled in as `'x-run'?: string`,
// which an unquoted-only pattern cannot see.
var browserFieldLine = regexp.MustCompile(`^\s*['"]?([a-z_][a-z0-9_-]*)['"]?\??:`)

func browserEventFields(t *testing.T) []string {
	t.Helper()
	fields := browserFields(t, "RunEvent")
	if len(fields) < 10 {
		t.Fatalf("found only %d fields on the browser's RunEvent", len(fields))
	}
	return fields
}

// browserFields reads the property names off one of the browser's object types.
//
// Comments are stripped first: a field that exists only in prose - "blob: the
// daemon kept the whole body" inside a block comment - would otherwise count as
// declared, which is the one way a removed field passes unnoticed.
func browserFields(t *testing.T, name string) []string {
	t.Helper()
	source := stripComments(readBrowserWire(t))
	open := "export type " + name + " = {"
	start := strings.Index(source, open)
	if start < 0 {
		t.Fatalf("%s does not declare %s", browserWire, open)
	}
	rest := source[start+len(open):]
	// A one-line type ends at its brace; a multi-line one at a brace in the
	// first column.
	end := strings.Index(rest, "\n}")
	if brace := strings.Index(rest, "}"); brace >= 0 && (end < 0 || brace < end) {
		end = brace
	}
	if end < 0 {
		t.Fatalf("%s: the %s type is not closed", browserWire, name)
	}
	var fields []string
	// Split on both, so a one-line `{ a: X; b: Y }` reads as two declarations.
	for _, part := range strings.FieldsFunc(rest[:end], func(r rune) bool {
		return r == '\n' || r == ';'
	}) {
		if m := browserFieldLine.FindStringSubmatch(part); m != nil {
			fields = append(fields, m[1])
		}
	}
	if len(fields) == 0 {
		t.Fatalf("%s: found no fields on %s", browserWire, name)
	}
	sort.Strings(fields)
	return fields
}

// stripComments blanks out // and /* */ runs. It is not a TypeScript parser and
// does not need to be: it never sees a string literal containing a comment
// marker, because this file's declarations are types.
func stripComments(source string) string {
	var out strings.Builder
	for i := 0; i < len(source); {
		switch {
		case strings.HasPrefix(source[i:], "//"):
			end := strings.IndexByte(source[i:], '\n')
			if end < 0 {
				return out.String()
			}
			i += end
		case strings.HasPrefix(source[i:], "/*"):
			end := strings.Index(source[i:], "*/")
			if end < 0 {
				return out.String()
			}
			// The newlines are kept so line-based reading still lines up.
			out.WriteString(strings.Repeat("\n", strings.Count(source[i:i+end], "\n")))
			i += end + 2
		default:
			out.WriteByte(source[i])
			i++
		}
	}
	return out.String()
}

func readBrowserWire(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(browserWire))
	if err != nil {
		t.Fatalf("reading the browser's wire vocabulary: %v", err)
	}
	return string(b)
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
