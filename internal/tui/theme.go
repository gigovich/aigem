package tui

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/muesli/termenv"
)

// newGlamour builds a renderer for the given style at the given width, applying
// the shared color profile and chroma formatter.
func newGlamour(style ansi.StyleConfig, width int) *glamour.TermRenderer {
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithColorProfile(colorProfile),
		glamour.WithChromaFormatter(chromaFormatter()),
		glamour.WithWordWrap(max(20, width-4)),
	)
	if err != nil {
		return nil
	}
	return r
}

// cCard is the background for an assistant answer and the plan panel. It now
// matches cBase so those surfaces sit flat on the canvas - only the gutter bar and
// borders set them apart. cCodeBg is the dark well behind code snippets, sitting
// below cBase for contrast.
const (
	cCard   = cBase
	cCodeBg = cCrust
)

// colorProfile is the single source of truth for terminal color depth. We
// detect it once and force it on both Bubble Tea and glamour so neither degrades
// truecolor to the 256-color palette independently (glamour's chroma code
// highlighter otherwise defaults to terminal256 regardless of the profile).
var colorProfile = termenv.ColorProfile()

// teaColorProfile restates colorProfile in Bubble Tea's own profile type, so the
// frame Bubble Tea paints and the markdown glamour renders agree on color depth.
func teaColorProfile() colorprofile.Profile {
	switch colorProfile {
	case termenv.TrueColor:
		return colorprofile.TrueColor
	case termenv.ANSI256:
		return colorprofile.ANSI256
	case termenv.ANSI:
		return colorprofile.ANSI
	default:
		return colorprofile.Ascii
	}
}

// chromaFormatter picks the chroma code formatter matching the color depth so
// syntax highlighting is emitted at full fidelity when truecolor is available.
func chromaFormatter() string {
	if colorProfile == termenv.TrueColor {
		return "terminal16m"
	}
	return "terminal256"
}

// hexColor is one palette entry. It satisfies color.Color for lipgloss v2 styles
// while keeping its hex text, which glamour's StyleConfig wants as a plain string.
// Being a string type also keeps the palette declarable as constants.
type hexColor string

func (c hexColor) RGBA() (r, g, b, a uint32) { return lipgloss.Color(string(c)).RGBA() }

// col, strPtr, boolPtr, uintPtr build the pointer values glamour's StyleConfig
// expects, reusing the shared Catppuccin Mocha palette so colors live in one place.
func col(c hexColor) *string  { s := string(c); return &s }
func strPtr(s string) *string { return &s }
func boolPtr(v bool) *bool    { return &v }
func uintPtr(v uint) *uint    { return &v }

// Two Catppuccin Mocha themes for glamour, differing only in fill color.
var (
	// catppuccinProse renders prose on the raised card background. catppuccinCode
	// renders fenced blocks on surface0; padded to full width by renderMarkdown
	// it forms a solid code panel (glamour itself never pads code lines).
	catppuccinProse = buildCatppuccinStyle(cCard)
	catppuccinCode  = buildCatppuccinStyle(cCodeBg)
)

func buildCatppuccinStyle(bg hexColor) ansi.StyleConfig {
	fill := col(bg)
	st := ansi.StyleConfig{
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{BlockPrefix: "\n", BlockSuffix: "\n", Color: col(cText)},
			Margin:         uintPtr(0),
		},
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: col(cSubtext0)},
			Indent:         uintPtr(1),
			IndentToken:    strPtr("│ "),
		},
		List: ansi.StyleList{
			StyleBlock:  ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(cText)}},
			LevelIndent: 2,
		},
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{BlockSuffix: "\n", Color: col(cMauve), Bold: boolPtr(true)},
		},
		H1:             ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "# ", Color: col(cLavender), Bold: boolPtr(true)}},
		H2:             ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "## "}},
		H3:             ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "### "}},
		H4:             ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "#### "}},
		H5:             ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "##### "}},
		H6:             ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Prefix: "###### "}},
		Strikethrough:  ansi.StylePrimitive{CrossedOut: boolPtr(true)},
		Emph:           ansi.StylePrimitive{Italic: boolPtr(true)},
		Strong:         ansi.StylePrimitive{Bold: boolPtr(true)},
		HorizontalRule: ansi.StylePrimitive{Color: col(cSurface2), Format: "\n──────\n"},
		Item:           ansi.StylePrimitive{BlockPrefix: "• "},
		Enumeration:    ansi.StylePrimitive{BlockPrefix: ". ", Color: col(cBlue)},
		Task:           ansi.StyleTask{Ticked: "[✓] ", Unticked: "[ ] "},
		Link:           ansi.StylePrimitive{Color: col(cBlue), Underline: boolPtr(true)},
		LinkText:       ansi.StylePrimitive{Color: col(cSapphire)},
		Image:          ansi.StylePrimitive{Color: col(cBlue), Underline: boolPtr(true)},
		ImageText:      ansi.StylePrimitive{Color: col(cSapphire), Format: "Image: {{.text}} →"},
		Code:           ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(cPeach), Prefix: " ", Suffix: " "}},
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: col(cText)}, Margin: uintPtr(0)},
			Chroma: &ansi.Chroma{
				Text:                ansi.StylePrimitive{Color: col(cText)},
				Error:               ansi.StylePrimitive{Color: col(cText)},
				Comment:             ansi.StylePrimitive{Color: col(cOverlay0), Italic: boolPtr(true)},
				CommentPreproc:      ansi.StylePrimitive{Color: col(cMauve)},
				Keyword:             ansi.StylePrimitive{Color: col(cMauve)},
				KeywordReserved:     ansi.StylePrimitive{Color: col(cMauve)},
				KeywordNamespace:    ansi.StylePrimitive{Color: col(cMauve)},
				KeywordType:         ansi.StylePrimitive{Color: col(cYellow)},
				Operator:            ansi.StylePrimitive{Color: col(cTeal)},
				Punctuation:         ansi.StylePrimitive{Color: col(cSubtext1)},
				Name:                ansi.StylePrimitive{Color: col(cText)},
				NameBuiltin:         ansi.StylePrimitive{Color: col(cPeach)},
				NameTag:             ansi.StylePrimitive{Color: col(cMauve)},
				NameAttribute:       ansi.StylePrimitive{Color: col(cYellow)},
				NameClass:           ansi.StylePrimitive{Color: col(cYellow)},
				NameConstant:        ansi.StylePrimitive{Color: col(cPeach)},
				NameDecorator:       ansi.StylePrimitive{Color: col(cBlue)},
				NameFunction:        ansi.StylePrimitive{Color: col(cBlue)},
				LiteralNumber:       ansi.StylePrimitive{Color: col(cPeach)},
				LiteralString:       ansi.StylePrimitive{Color: col(cGreen)},
				LiteralStringEscape: ansi.StylePrimitive{Color: col(cPink)},
				GenericDeleted:      ansi.StylePrimitive{Color: col(cRed)},
				GenericEmph:         ansi.StylePrimitive{Italic: boolPtr(true)},
				GenericInserted:     ansi.StylePrimitive{Color: col(cGreen)},
				GenericStrong:       ansi.StylePrimitive{Bold: boolPtr(true)},
				GenericSubheading:   ansi.StylePrimitive{Color: col(cMauve)},
				Background:          ansi.StylePrimitive{},
			},
		},
		Table:                 ansi.StyleTable{StyleBlock: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{}}},
		DefinitionDescription: ansi.StylePrimitive{BlockPrefix: "\n🠶 "},
	}

	// Fill the background on every element so the rendered block is a gap-free
	// rectangle (glamour resets to the terminal default otherwise). Inline code
	// keeps a distinct surface0 chip.
	for _, p := range prosePrimitives(&st) {
		p.BackgroundColor = fill
	}
	st.Code.BackgroundColor = col(cSurface0)
	st.CodeBlock.BackgroundColor = fill
	for _, p := range chromaPrimitives(st.CodeBlock.Chroma) {
		p.BackgroundColor = fill
	}
	return st
}

// prosePrimitives returns every non-code StylePrimitive in st so a background
// can be applied uniformly.
func prosePrimitives(st *ansi.StyleConfig) []*ansi.StylePrimitive {
	return []*ansi.StylePrimitive{
		&st.Document.StylePrimitive, &st.BlockQuote.StylePrimitive, &st.Paragraph.StylePrimitive,
		&st.List.StylePrimitive, &st.Heading.StylePrimitive,
		&st.H1.StylePrimitive, &st.H2.StylePrimitive, &st.H3.StylePrimitive,
		&st.H4.StylePrimitive, &st.H5.StylePrimitive, &st.H6.StylePrimitive,
		&st.Strikethrough, &st.Emph, &st.Strong, &st.HorizontalRule, &st.Item, &st.Enumeration,
		&st.Task.StylePrimitive, &st.Link, &st.LinkText, &st.Image, &st.ImageText, &st.Text,
		&st.Table.StylePrimitive, &st.DefinitionList.StylePrimitive,
		&st.DefinitionTerm, &st.DefinitionDescription,
	}
}

// chromaPrimitives returns every chroma token style so a code background can be
// applied uniformly, including the Background token that backs blank lines.
func chromaPrimitives(c *ansi.Chroma) []*ansi.StylePrimitive {
	return []*ansi.StylePrimitive{
		&c.Text, &c.Error, &c.Comment, &c.CommentPreproc, &c.Keyword, &c.KeywordReserved,
		&c.KeywordNamespace, &c.KeywordType, &c.Operator, &c.Punctuation, &c.Name,
		&c.NameBuiltin, &c.NameTag, &c.NameAttribute, &c.NameClass, &c.NameConstant,
		&c.NameDecorator, &c.NameFunction, &c.LiteralNumber, &c.LiteralString,
		&c.LiteralStringEscape, &c.GenericDeleted, &c.GenericEmph, &c.GenericInserted,
		&c.GenericStrong, &c.GenericSubheading, &c.Background,
	}
}
