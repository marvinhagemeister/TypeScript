package editprojection

import (
	"fmt"
	"strings"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/parser"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
)

// Measures planning only: parsing, semantic collection, RPC and IO are excluded.
func BenchmarkRenamePlan(b *testing.B) {
	for _, mode := range []string{"single-segment", "per-token", "derived"} {
		for _, count := range []int{100, 1000, 3000} {
			b.Run(fmt.Sprintf("%s/%d", mode, count), func(b *testing.B) {
				original := strings.Repeat("x;\n", count)
				virtual, stride := original, 3
				if mode == "derived" {
					virtual = strings.Repeat("x;onX;\n", count)
					stride = 7
				}
				file := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/bench.view", PathKey: "/bench.view"}, virtual, core.ScriptKindTS)
				var segments []spanmap.Segment
				edits := make([]SourceEdit, count)
				for i := range count {
					start := core.TextPos(i * stride)
					origin := core.TextPos(i * 3)
					edits[i] = SourceEdit{File: file, Change: core.TextChange{TextRange: core.NewTextRange(int(start), int(start)+1), NewText: "y"}}
					if mode != "single-segment" {
						segments = append(segments, spanmap.Segment{VirtualStart: start, VirtualEnd: start + 1, OriginalStart: origin, OriginalEnd: origin + 1, Kind: spanmap.KindVerbatim, Features: spanmap.FeatureAll})
					}
					if mode == "derived" {
						segments = append(segments, spanmap.Segment{VirtualStart: start + 2, VirtualEnd: start + 5, OriginalStart: origin, OriginalEnd: origin + 1, Kind: spanmap.KindAtom, Features: spanmap.FeatureNone})
					}
				}
				if mode == "single-segment" {
					segments = []spanmap.Segment{{VirtualEnd: core.TextPos(len(virtual)), OriginalEnd: core.TextPos(len(original)), Kind: spanmap.KindVerbatim, Features: spanmap.FeatureAll}}
				}
				file.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "benchmark", OriginalText: original, SpanMap: spanmap.New(segments), TransformIdentity: "benchmark-v1"})
				view := SourceProjection{File: file, DerivedRename: mode == "derived"}
				b.ReportAllocs()
				for b.Loop() {
					if _, err := NewPlan(Snapshot{ID: "benchmark"}, Operation{Kind: Rename, NewName: "y"}, edits, view); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
