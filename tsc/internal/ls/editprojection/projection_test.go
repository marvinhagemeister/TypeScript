package editprojection

import (
	"context"
	"errors"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/parser"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
	"gotest.tools/v3/assert"
)

func TestProjectionRequiresCompleteCoverage(t *testing.T) {
	t.Parallel()
	file := mappedFile("save-item", "saveItem", spanmap.KindAtom)
	plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename, NewName: "nextItem"}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}})
	assert.NilError(t, err)
	result, err := plan.Project(t.Context(), map[string]Provider{"test": func(ctx context.Context, req Request) (Response, error) {
		return Response{Snapshot: req.Snapshot}, nil
	}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF16)
	assert.Assert(t, err != nil)
	assert.Assert(t, result == nil)
}

func TestProjectionRejectsInvalidResponses(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing", "duplicate", "unknown", "foreign", "bounds", "conflict", "generated-only", "stale-response", "failure", "cancel", "stale-host", "mutated-request"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file := mappedFile("save-item", "saveItem", spanmap.KindAtom)
			plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename, NewName: "nextItem"}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}})
			assert.NilError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			current := "1"
			provider := func(ctx context.Context, req Request) (Response, error) {
				result := Result{Inputs: []int{0}, Edits: []AuthoredEdit{{Document: 0, Change: change(0, 9, "next-item")}}}
				resp := Response{Snapshot: "1", Results: []Result{result}}
				switch name {
				case "missing":
					resp.Results = nil
				case "duplicate":
					resp.Results = append(resp.Results, result)
				case "unknown":
					resp.Results[0].Inputs = []int{99}
				case "foreign":
					resp.Results[0].Edits[0].Document = 99
				case "bounds":
					resp.Results[0].Edits[0].Change = change(0, 100, "bad")
				case "conflict":
					resp.Results[0].Edits = append(resp.Results[0].Edits, AuthoredEdit{Document: 0, Change: change(0, 9, "different")})
				case "generated-only":
					resp.Results[0] = Result{Inputs: []int{0}, GeneratedOnly: true, Reason: "not really generated"}
				case "stale-response":
					resp.Snapshot = "old"
				case "failure":
					return Response{}, errors.New("mapper failed")
				case "cancel":
					cancel()
				case "stale-host":
					current = "2"
				case "mutated-request":
					req.Documents[0].Text = "a much longer replacement document"
					resp.Results[0].Edits[0].Change = change(0, 20, "bad")
				}
				return resp, nil
			}
			result, err := plan.Project(ctx, map[string]Provider{"test": provider}, func() string { return current }, lsproto.PositionEncodingKindUTF16)
			assert.Assert(t, err != nil, name)
			assert.Assert(t, result == nil, name)
		})
	}
}

func TestProjectionDeduplicatesAndVersionsEdits(t *testing.T) {
	t.Parallel()
	file := mappedFile("save-item", "saveItem", spanmap.KindAtom)
	supplemental := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/app.vue.0.ts", PathKey: "/app.vue.0.ts"}, file.Text(), core.ScriptKindTS)
	supplemental.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "test", OriginalText: file.OriginalText(), SpanMap: file.SpanMap(), CanonicalSourceFile: file, TransformIdentity: "mapper-v1"})
	plan, err := NewPlan(Snapshot{ID: "1", Versions: map[string]int32{"/app.vue": 7}}, Operation{Kind: Rename, NewName: "nextItem"}, []SourceEdit{
		{File: file, Change: change(0, 8, "nextItem")},
		{File: supplemental, Change: change(0, 8, "nextItem")},
	})
	assert.NilError(t, err)
	calls := 0
	result, err := plan.Project(t.Context(), map[string]Provider{"test": func(ctx context.Context, req Request) (Response, error) {
		calls++
		assert.Equal(t, len(req.Projections), 2)
		assert.Equal(t, len(req.Documents), 1)
		var results []Result
		for _, edit := range req.Edits {
			results = append(results, Result{Inputs: []int{edit.ID}, Edits: []AuthoredEdit{{Document: 0, Change: change(0, 9, "next-item")}}})
		}
		return Response{Snapshot: req.Snapshot, Results: results}, nil
	}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF16)
	assert.NilError(t, err)
	assert.Equal(t, calls, 1)
	assert.Equal(t, len(*result.DocumentChanges), 1)
	doc := (*result.DocumentChanges)[0].TextDocumentEdit
	assert.Equal(t, doc.TextDocument.Uri, lsproto.DocumentUri("file:///app.vue"))
	assert.Equal(t, *doc.TextDocument.Version.Integer, int32(7))
	assert.Equal(t, len(doc.Edits), 1)
	assert.Equal(t, doc.Edits[0].TextEdit.NewText, "next-item")
}

func TestProjectionExactFallbackAndUnicode(t *testing.T) {
	t.Parallel()
	for _, kind := range []spanmap.Kind{spanmap.KindVerbatim, spanmap.KindAtom} {
		file := mappedFile("😀saveItem", "😀saveItem", kind)
		plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename, NewName: "nextItem"}, []SourceEdit{{File: file, Change: change(4, 12, "nextItem")}})
		assert.NilError(t, err)
		result, err := plan.Project(t.Context(), nil, func() string { return "1" }, lsproto.PositionEncodingKindUTF16)
		if kind == spanmap.KindAtom {
			assert.Assert(t, err != nil)
			assert.Assert(t, result == nil)
		} else {
			assert.NilError(t, err)
			edit := (*result.DocumentChanges)[0].TextDocumentEdit.Edits[0].TextEdit
			assert.Equal(t, edit.Range.Start.Character, uint32(2))
		}
	}
}

func TestProjectionGeneratedOnlyAndMappedIslands(t *testing.T) {
	t.Parallel()
	for _, hasOrigin := range []bool{false, true} {
		file := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/app.vue", PathKey: "/app.vue"}, "before; name; after;", core.ScriptKindTS)
		var segments []spanmap.Segment
		if hasOrigin {
			segments = []spanmap.Segment{{VirtualStart: 8, VirtualEnd: 12, OriginalEnd: 4, Kind: spanmap.KindVerbatim, Features: spanmap.FeatureAll}}
		}
		file.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "test", OriginalText: "name", SpanMap: spanmap.New(segments)})
		plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: OrganizeImports}, []SourceEdit{{File: file, Change: change(0, len(file.Text()), "")}})
		assert.NilError(t, err)
		result, err := plan.Project(t.Context(), map[string]Provider{"test": func(ctx context.Context, req Request) (Response, error) {
			return Response{Snapshot: req.Snapshot, Results: []Result{{Inputs: []int{0}, GeneratedOnly: true, Reason: "helper is regenerated"}}}, nil
		}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF16)
		if hasOrigin {
			assert.ErrorContains(t, err, "authored occurrence")
			assert.Assert(t, result == nil)
		} else {
			assert.NilError(t, err)
			assert.Equal(t, len(*result.DocumentChanges), 0)
		}
	}
}

func TestProjectionRejectsInvalidBoundariesAndCoincidentInsertions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		edits []core.TextChange
	}{
		{"split character", []core.TextChange{change(1, 2, "x")}},
		{"split newline", []core.TextChange{change(5, 5, "x")}},
		{"negative", []core.TextChange{change(-1, 0, "x")}},
		{"reversed", []core.TextChange{change(4, 0, "x")}},
		{"coincident inserts", []core.TextChange{change(0, 0, "a"), change(0, 0, "b")}},
		{"overlap", []core.TextChange{change(0, 7, "a"), change(6, 8, "b")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			file := mappedFile("😀\r\nname", "name", spanmap.KindAtom)
			plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename, NewName: "next"}, []SourceEdit{{File: file, Change: change(0, 4, "next")}})
			assert.NilError(t, err)
			result, err := plan.Project(t.Context(), map[string]Provider{"test": func(ctx context.Context, req Request) (Response, error) {
				var edits []AuthoredEdit
				for _, edit := range test.edits {
					edits = append(edits, AuthoredEdit{Document: 0, Change: edit})
				}
				return Response{Snapshot: req.Snapshot, Results: []Result{{Inputs: []int{0}, Edits: edits}}}, nil
			}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF16)
			assert.Assert(t, err != nil)
			assert.Assert(t, result == nil)
		})
	}
}

func TestProjectionCancellationAndStalenessBeforeProvider(t *testing.T) {
	t.Parallel()
	file := mappedFile("save-item", "saveItem", spanmap.KindAtom)
	plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename, NewName: "nextItem"}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}})
	assert.NilError(t, err)
	calls := 0
	providers := map[string]Provider{"test": func(context.Context, Request) (Response, error) { calls++; return Response{}, nil }}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := plan.Project(ctx, providers, func() string { return "1" }, lsproto.PositionEncodingKindUTF16)
	assert.Assert(t, errors.Is(err, context.Canceled))
	assert.Assert(t, result == nil)
	result, err = plan.Project(t.Context(), providers, func() string { return "2" }, lsproto.PositionEncodingKindUTF16)
	assert.ErrorContains(t, err, "stale snapshot")
	assert.Assert(t, result == nil)
	assert.Equal(t, calls, 0)
}

func TestProjectionRejectsEditingAnotherOwnersDocument(t *testing.T) {
	t.Parallel()
	file := mappedFile("save-item", "saveItem", spanmap.KindAtom)
	plain := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/plain.ts", PathKey: "/plain.ts"}, "saveItem", core.ScriptKindTS)
	plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename, NewName: "nextItem"}, []SourceEdit{
		{File: file, Change: change(0, 8, "nextItem")},
		{File: plain, Change: change(0, 8, "nextItem")},
	})
	assert.NilError(t, err)
	result, err := plan.Project(t.Context(), map[string]Provider{"test": func(ctx context.Context, req Request) (Response, error) {
		assert.Equal(t, len(req.Documents), 1)
		return Response{Snapshot: req.Snapshot, Results: []Result{{Inputs: []int{0}, Edits: []AuthoredEdit{{Document: 1, Change: change(0, 8, "bad")}}}}}, nil
	}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF16)
	assert.ErrorContains(t, err, "unauthorized")
	assert.Assert(t, result == nil)
}

func change(start, end int, text string) core.TextChange {
	return core.TextChange{TextRange: core.NewTextRange(start, end), NewText: text}
}

func mappedFile(original, virtual string, kind spanmap.Kind) *ast.SourceFile {
	file := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/app.vue", PathKey: "/app.vue"}, virtual, core.ScriptKindTS)
	file.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{
		ContentMapper: "test", OriginalText: original, TransformIdentity: "mapper-v1",
		SpanMap: spanmap.New([]spanmap.Segment{{VirtualEnd: core.TextPos(len(virtual)), OriginalEnd: core.TextPos(len(original)), Kind: kind, Features: spanmap.FeatureAll}}),
	})
	return file
}
