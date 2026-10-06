package editprojection

import (
	"context"
	"slices"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/parser"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
	"gotest.tools/v3/assert"
)

func derivedFile() *ast.SourceFile {
	file := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/app.vue", PathKey: "/app.vue"}, "saveItem onSaveItem other", core.ScriptKindTS)
	file.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "test", OriginalText: "save-item other", TransformIdentity: "mapper-v1", SpanMap: spanmap.New([]spanmap.Segment{
		{VirtualStart: 0, VirtualEnd: 8, OriginalStart: 0, OriginalEnd: 9, Kind: spanmap.KindAtom, Features: spanmap.FeatureAll},
		{VirtualStart: 9, VirtualEnd: 19, OriginalStart: 0, OriginalEnd: 9, Kind: spanmap.KindAtom, Features: spanmap.FeatureNone},
		{VirtualStart: 20, VirtualEnd: 25, OriginalStart: 10, OriginalEnd: 15, Kind: spanmap.KindVerbatim, Features: spanmap.FeatureAll},
	})})
	return file
}

func TestDerivedRenameAccounting(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"success", "missing", "duplicate", "unknown", "unrooted", "untouched", "widened", "supplemental", "generated-only", "stale", "cancel", "mutated-request", "foreign-document", "negative-document", "out-of-bounds"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			file := derivedFile()
			plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename, NewName: "nextItem"}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}}, SourceProjection{File: file, DerivedRename: true})
			assert.NilError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result, err := plan.Project(ctx, map[string]Provider{"test": func(_ context.Context, req Request) (Response, error) {
				assert.Equal(t, len(req.Effects), 1)
				assert.Equal(t, req.Effects[0].Span, core.NewTextRange(9, 19))
				assert.DeepEqual(t, req.Effects[0].Inputs, []int{0})
				coverage := Result{Inputs: []int{0}, DerivedEffects: []int{req.Effects[0].ID}, Edits: []AuthoredEdit{{Document: 0, Change: change(0, 9, "next-item")}}}
				response := Response{Snapshot: req.Snapshot}
				switch mode {
				case "foreign-document":
					coverage.Edits = append(coverage.Edits, AuthoredEdit{Document: 99, Change: change(0, 0, "bad")})
				case "negative-document":
					coverage.Edits = append(coverage.Edits, AuthoredEdit{Document: -1, Change: change(0, 0, "bad")})
				case "out-of-bounds":
					coverage.Edits = append(coverage.Edits, AuthoredEdit{Document: 0, Change: change(15, 100, "bad")})
				case "missing":
					coverage.DerivedEffects = nil
				case "duplicate":
					coverage.DerivedEffects = append(coverage.DerivedEffects, coverage.DerivedEffects[0])
				case "unknown":
					coverage.DerivedEffects = []int{99}
				case "unrooted":
					coverage.Inputs = []int{99}
				case "untouched":
					coverage.Edits[0].Change = change(10, 15, "next")
				case "widened":
					coverage.Edits[0].Change = change(0, 15, "next-item next")
				case "supplemental":
					coverage.Edits = append(coverage.Edits, AuthoredEdit{Document: 0, Change: change(10, 15, "next")})
				case "generated-only":
					coverage.GeneratedOnly = true
					coverage.Reason = "derived"
					coverage.Edits = nil
				case "stale":
					response.Snapshot = "old"
				case "cancel":
					cancel()
				case "mutated-request":
					req.Effects[0].Span = core.NewTextRange(20, 25)
					req.Effects[0].Inputs[0] = 99
					coverage.Edits[0].Change = change(10, 15, "next")
				}
				response.Results = []Result{coverage}
				return response, nil
			}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF16)
			if mode == "success" {
				assert.NilError(t, err)
				assert.Equal(t, (*result.DocumentChanges)[0].TextDocumentEdit.Edits[0].TextEdit.NewText, "next-item")
			} else {
				assert.Assert(t, err != nil, mode)
				assert.Assert(t, result == nil)
			}
		})
	}
}

func TestDerivedRenameRequiresOptInAndProvider(t *testing.T) {
	t.Parallel()
	file := derivedFile()
	_, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}})
	assert.ErrorContains(t, err, "uncovered rename projection")
	plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}}, SourceProjection{File: file, DerivedRename: true})
	assert.NilError(t, err)
	result, err := plan.Project(t.Context(), nil, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
	assert.Assert(t, result == nil)
	assert.Assert(t, err != nil)
}

func TestDerivedRenameCannotEscapeThroughExactFallback(t *testing.T) {
	t.Parallel()
	file := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/a.view", PathKey: "/a.view"}, "name onName", core.ScriptKindTS)
	file.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "test", OriginalText: "name", SpanMap: spanmap.New([]spanmap.Segment{
		{VirtualEnd: 4, OriginalEnd: 4, Kind: spanmap.KindVerbatim, Features: spanmap.FeatureAll},
		{VirtualStart: 5, VirtualEnd: 11, OriginalEnd: 4, Kind: spanmap.KindAtom, Features: spanmap.FeatureNone},
	})})
	plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: file, Change: change(0, 4, "next")}}, SourceProjection{File: file, DerivedRename: true})
	assert.NilError(t, err)
	result, err := plan.Project(t.Context(), nil, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
	assert.Assert(t, result == nil)
	assert.ErrorContains(t, err, "no provider for required derived effect")
}

func TestDerivedRenameWideningTouchesEditFreeView(t *testing.T) {
	t.Parallel()
	file := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/app.vue", PathKey: "/app.vue"}, "saveItem", core.ScriptKindTS)
	file.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "test", TransformIdentity: "mapper-v1", OriginalText: "save-item other", SpanMap: spanmap.New([]spanmap.Segment{{VirtualEnd: 8, OriginalEnd: 9, Kind: spanmap.KindAtom, Features: spanmap.FeatureAll}})})
	extra := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/app.vue.0.ts", PathKey: "/app.vue.0.ts"}, "other", core.ScriptKindTS)
	extra.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "test", TransformIdentity: "mapper-v1", OriginalText: file.OriginalText(), CanonicalSourceFile: file, SpanMap: spanmap.New([]spanmap.Segment{{VirtualEnd: 5, OriginalStart: 10, OriginalEnd: 15, Kind: spanmap.KindAlias, Features: spanmap.FeatureNone}})})
	plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}}, SourceProjection{File: file, DerivedRename: true}, SourceProjection{File: extra, DerivedRename: true})
	assert.NilError(t, err)
	result, err := plan.Project(t.Context(), map[string]Provider{"test": func(_ context.Context, req Request) (Response, error) {
		assert.Equal(t, len(req.Effects), 0)
		return Response{Snapshot: req.Snapshot, Results: []Result{{Inputs: []int{0}, Edits: []AuthoredEdit{{Document: 0, Change: change(0, 15, "next-item next")}}}}}, nil
	}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
	assert.Assert(t, result == nil)
	assert.ErrorContains(t, err, "unaccounted authored edit impact")
}

func TestRenameRejectsConflictingProjectViews(t *testing.T) {
	t.Parallel()
	file := mappedFile("save-item", "saveItem", spanmap.KindAtom)
	// Same AST and transform identity, different editing contexts: never choose an arbitrary handle.
	_, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: file, Context: "first", Change: change(0, 8, "nextItem")}},
		SourceProjection{File: file, Context: "first", Owner: "first", DerivedRename: true}, SourceProjection{File: file, Context: "second", Owner: "second", DerivedRename: true})
	assert.ErrorContains(t, err, "inconsistent projections")
}

func TestDerivedRenameAliasAndHiddenPartialProjection(t *testing.T) {
	t.Parallel()
	for _, kind := range []spanmap.Kind{spanmap.KindAtom, spanmap.KindAlias} {
		file := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/app.vue", PathKey: "/app.vue"}, "saveItem derived", core.ScriptKindTS)
		file.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "test", OriginalText: "save-item", SpanMap: spanmap.New([]spanmap.Segment{
			{VirtualEnd: 8, OriginalEnd: 9, Kind: kind, Features: spanmap.FeatureAll},
			{VirtualStart: 9, VirtualEnd: 16, OriginalStart: 2, OriginalEnd: 5, Kind: kind, Features: spanmap.FeatureNone},
		})})
		plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}}, SourceProjection{File: file, DerivedRename: true})
		assert.NilError(t, err)
		_, err = plan.Project(t.Context(), map[string]Provider{"test": func(_ context.Context, req Request) (Response, error) {
			assert.Equal(t, len(req.Effects), 1)
			return Response{Snapshot: req.Snapshot, Results: []Result{{Inputs: []int{0}, DerivedEffects: []int{req.Effects[0].ID}, Edits: []AuthoredEdit{{Document: 0, Change: change(0, 9, "next-item")}}}}}, nil
		}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
		assert.NilError(t, err)
		assert.Equal(t, file.SpanMap().Segments()[1].Kind, kind)
		assert.Equal(t, file.SpanMap().Segments()[1].Features, spanmap.FeatureNone)
	}
}

func TestRenameImpactIncludesInsertionEdges(t *testing.T) {
	t.Parallel()
	projection := Projection{Mapped: true, Mappings: []spanmap.Segment{
		{VirtualEnd: 4, OriginalEnd: 4, Kind: spanmap.KindVerbatim},
		{VirtualStart: 5, VirtualEnd: 9, OriginalStart: 4, OriginalEnd: 8, Kind: spanmap.KindAtom},
		{VirtualStart: 10, VirtualEnd: 11, OriginalStart: 4, OriginalEnd: 4, Kind: spanmap.KindAlias},
	}}
	assert.Assert(t, slices.Equal(affectedSpans(projection, core.NewTextRange(4, 4)), []core.TextRange{core.NewTextRange(4, 4), core.NewTextRange(5, 9), core.NewTextRange(10, 11)}))
	assert.Assert(t, slices.Equal(affectedSpans(projection, core.NewTextRange(4, 5)), []core.TextRange{core.NewTextRange(5, 9), core.NewTextRange(10, 11)}))
}

func TestDerivedEffectRequiresItsOwnSemanticRoot(t *testing.T) {
	t.Parallel()
	file := derivedFile()
	plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}, {File: file, Change: change(20, 25, "nextItem")}}, SourceProjection{File: file, DerivedRename: true})
	assert.NilError(t, err)
	result, err := plan.Project(t.Context(), map[string]Provider{"test": func(_ context.Context, req Request) (Response, error) {
		return Response{Snapshot: req.Snapshot, Results: []Result{
			{Inputs: []int{0}, Edits: []AuthoredEdit{{Document: 0, Change: change(0, 9, "next-item")}}},
			{Inputs: []int{1}, DerivedEffects: []int{0}, Edits: []AuthoredEdit{{Document: 0, Change: change(0, 9, "next-item")}}},
		}}, nil
	}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
	assert.Assert(t, result == nil)
	assert.ErrorContains(t, err, "not rooted")
}

func TestDerivedEffectCannotAcknowledgeAnotherOwner(t *testing.T) {
	t.Parallel()
	first := derivedFile()
	second := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/other.vue", PathKey: "/other.vue"}, first.Text(), core.ScriptKindTS)
	second.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "test", OriginalText: first.OriginalText(), SpanMap: first.SpanMap(), TransformIdentity: "mapper-v1"})
	plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: first, Change: change(0, 8, "nextItem")}, {File: second, Change: change(0, 8, "nextItem")}}, SourceProjection{File: first, Owner: "first", DerivedRename: true}, SourceProjection{File: second, Owner: "second", DerivedRename: true})
	assert.NilError(t, err)
	provider := func(_ context.Context, req Request) (Response, error) {
		assert.Equal(t, req.Effects[0].ID, 0)
		return Response{Snapshot: req.Snapshot, Results: []Result{{Inputs: []int{0}, DerivedEffects: []int{1}, Edits: []AuthoredEdit{{Document: 0, Change: change(0, 9, "next-item")}}}}}, nil
	}
	result, err := plan.Project(t.Context(), map[string]Provider{"first": provider, "second": func(context.Context, Request) (Response, error) {
		t.Error("second provider must not run")
		return Response{}, nil
	}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
	assert.Assert(t, result == nil)
	assert.ErrorContains(t, err, "unknown or duplicate derived effect")
}

func TestRenameRejectsContradictoryProjectionAuthority(t *testing.T) {
	t.Parallel()
	file := derivedFile()
	_, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}}, SourceProjection{File: file, Owner: "first", DerivedRename: true}, SourceProjection{File: file, Owner: "second", DerivedRename: true})
	assert.ErrorContains(t, err, "inconsistent projection context")
	_, err = NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}}, SourceProjection{File: file, DerivedRename: true}, SourceProjection{File: file})
	assert.ErrorContains(t, err, "inconsistent projection context")
}

func TestRenameIncludesEditFreeProjection(t *testing.T) {
	t.Parallel()
	file := mappedFile("save-item", "saveItem", spanmap.KindAtom)
	extra := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: "/app.vue.0.ts", PathKey: "/app.vue.0.ts"}, "onSaveItem", core.ScriptKindTS)
	extra.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{ContentMapper: "test", OriginalText: file.OriginalText(), CanonicalSourceFile: file, TransformIdentity: "mapper-v1", SpanMap: spanmap.New([]spanmap.Segment{{VirtualEnd: 10, OriginalEnd: 9, Kind: spanmap.KindAtom, Features: spanmap.FeatureNone}})})
	edits := []SourceEdit{{File: file, Change: change(0, 8, "nextItem")}}
	_, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, edits, SourceProjection{File: extra})
	assert.ErrorContains(t, err, "uncovered rename projection")
	plan, err := NewPlan(Snapshot{ID: "1"}, Operation{Kind: Rename}, edits, SourceProjection{File: file, DerivedRename: true}, SourceProjection{File: extra, DerivedRename: true})
	assert.NilError(t, err)
	_, err = plan.Project(t.Context(), map[string]Provider{"test": func(_ context.Context, req Request) (Response, error) {
		assert.Equal(t, len(req.Projections), 2)
		assert.Equal(t, len(req.Effects), 1)
		assert.Equal(t, req.Effects[0].Projection, 1)
		return Response{Snapshot: req.Snapshot, Results: []Result{{Inputs: []int{0}, DerivedEffects: []int{req.Effects[0].ID}, Edits: []AuthoredEdit{{Document: 0, Change: change(0, 9, "next-item")}}}}}, nil
	}}, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
	assert.NilError(t, err)
}
