package contentmappertest

import (
	"strings"
	"unicode"

	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
)

// ListenerTransform maps authored camel/kebab listeners to whole-token Atoms. Duplicate
// projections reference the same symbol unless ambiguous selects a different second target.
func ListenerTransform(duplicate, ambiguous bool) func(string) contentmapper.Result {
	return func(original string) contentmapper.Result {
		var text strings.Builder
		if ambiguous {
			text.WriteString("import { child, other } from \"./child\";\n")
		} else {
			text.WriteString("import { child } from \"./child\";\n")
		}
		var segments []spanmap.Segment
		for offset := 0; offset < len(original); {
			rel := strings.IndexByte(original[offset:], '@')
			if rel < 0 {
				break
			}
			start := offset + rel + 1
			end := start + strings.IndexByte(original[start:], '=')
			name := original[start:end]
			parts := strings.Split(name, "-")
			for i := 1; i < len(parts); i++ {
				parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
			}
			canonical := strings.Join(parts, "")
			count := 1
			if duplicate {
				count = 2
			}
			for i := range count {
				if ambiguous && i == 1 {
					text.WriteString("other.")
				} else {
					text.WriteString("child.")
				}
				virtualStart := text.Len()
				text.WriteString(canonical)
				segments = append(segments, spanmap.Segment{VirtualStart: core.TextPos(virtualStart), VirtualEnd: core.TextPos(text.Len()), OriginalStart: core.TextPos(start), OriginalEnd: core.TextPos(end), Kind: spanmap.KindAtom, Features: spanmap.FeatureAll})
				text.WriteString(";\n")
			}
			offset = end + 1
		}
		return contentmapper.Result{Text: text.String(), VirtualExtension: ".ts", Mappings: spanmap.New(segments)}
	}
}

// ImportTransform collapses an authored import to an Atom, adds a generated helper import,
// and maps the body verbatim. Edit providers must retain authored comments and layout.
func ImportTransform(original string) contentmapper.Result {
	start := strings.Index(original, "import {")
	end := start + strings.Index(original[start:], ";") + 1
	importText := strings.Join(strings.Fields(original[start:end]), " ")
	prefix := "import { helper } from \"./runtime\";\n\n"
	text := prefix + original[:start] + importText + "\n\n"
	virtualStart := len(prefix) + start
	segments := []spanmap.Segment{{VirtualStart: core.TextPos(virtualStart), VirtualEnd: core.TextPos(virtualStart + len(importText)), OriginalStart: core.TextPos(start), OriginalEnd: core.TextPos(end), Kind: spanmap.KindAtom, Features: spanmap.FeatureAll}}
	bodyStart := strings.Index(original, "@body ") + len("@body ")
	segments = append(segments, spanmap.Segment{VirtualStart: core.TextPos(len(text)), VirtualEnd: core.TextPos(len(text) + len(original) - bodyStart), OriginalStart: core.TextPos(bodyStart), OriginalEnd: core.TextPos(len(original)), Kind: spanmap.KindVerbatim, Features: spanmap.FeatureAll})
	return contentmapper.Result{Text: text + original[bodyStart:], VirtualExtension: ".ts", Mappings: spanmap.New(segments)}
}

// RenameListener preserves the authored listener's camel/kebab spelling.
func RenameListener(original, name string) string {
	if !strings.Contains(original, "-") {
		return name
	}
	var kebab strings.Builder
	for _, ch := range name {
		if unicode.IsUpper(ch) {
			kebab.WriteByte('-')
			ch = unicode.ToLower(ch)
		}
		kebab.WriteRune(ch)
	}
	return kebab.String()
}
