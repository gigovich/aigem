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

// The approval a person answers, and the answer they give. Both cross the wire
// inside an event, and both are read by name in the browser.
func TestTheBrowserKnowsTheApprovalFields(t *testing.T) {
	for _, name := range []string{"Approval", "Option", "Client"} {
		want := goJSONTags(t, name)
		source := readBrowserWire(t)
		for _, field := range want {
			// Matched as a property declaration - preceded by a brace, a
			// semicolon or whitespace - so a field that only appears inside a
			// longer word or in prose does not pass. A one-line object type is
			// as valid a declaration as an indented one.
			if !regexp.MustCompile(`[{;\s]` + regexp.QuoteMeta(field) + `\??:`).MatchString(source) {
				t.Errorf("the browser does not read %s.%s; add it to %s", name, field, browserWire)
			}
		}
	}
}

// goJSONTags reads the json tag names off one struct in this package.
func goJSONTags(t *testing.T, name string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}
	var out []string
	for _, p := range pkg {
		for _, file := range p.Files {
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
	}
	if len(out) == 0 {
		t.Fatalf("found no json tags on %s", name)
	}
	sort.Strings(out)
	return out
}

var browserFieldLine = regexp.MustCompile(`^\s+([a-z_]+)\??:`)

// browserEventFields reads the property names off the browser's RunEvent.
func browserEventFields(t *testing.T) []string {
	t.Helper()
	source := readBrowserWire(t)
	const open = "export type RunEvent = {"
	start := strings.Index(source, open)
	if start < 0 {
		t.Fatalf("%s does not declare %s", browserWire, open)
	}
	rest := source[start+len(open):]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("%s: the RunEvent type is not closed", browserWire)
	}
	var fields []string
	for _, line := range strings.Split(rest[:end], "\n") {
		if m := browserFieldLine.FindStringSubmatch(line); m != nil {
			fields = append(fields, m[1])
		}
	}
	if len(fields) < 10 {
		t.Fatalf("found only %d fields on the browser's RunEvent", len(fields))
	}
	sort.Strings(fields)
	return fields
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
