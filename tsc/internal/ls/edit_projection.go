package ls

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/astnav"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/ls/editprojection"
	"github.com/microsoft/TypeScript/tsc/internal/ls/lsutil"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
)

// PrepareProjectedRename is an experimental, opt-in counterpart to GetRenameInfo. It permits a
// whole-token Atom, without changing its location fidelity or the legacy LSP entry point.
func (l *LanguageService) PrepareProjectedRename(ctx context.Context, uri lsproto.DocumentUri, position lsproto.Position) (RenameInfo, error) {
	_, _, info, err := l.resolveProjectedRename(ctx, uri, position)
	return info, err
}

func (l *LanguageService) resolveProjectedRename(ctx context.Context, uri lsproto.DocumentUri, position lsproto.Position) (*ast.SourceFile, *ast.Node, RenameInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, RenameInfo{}, err
	}
	program, file := l.tryGetProgramAndFile(uri.FileName())
	if file == nil {
		return nil, nil, RenameInfo{}, errors.New("edit projection: source file not found")
	}
	var selectedFile *ast.SourceFile
	var selectedNode *ast.Node
	var selectedSymbol *ast.Symbol
	var selectedInfo RenameInfo
	for _, mapped := range l.converters.FromLSPPositionForSourceFile(file, position, spanmap.FeatureRename) {
		if !mapped.Fidelity.IsSingleSegment() {
			continue
		}
		node := astnav.GetTouchingPropertyName(mapped.Script, int(mapped.Position))
		node = getAdjustedLocation(node, true, mapped.Script)
		if !nodeIsEligibleForRename(node) || ast.IsStringLiteralLike(node) && ast.TryGetImportFromModuleSpecifier(node) != nil {
			return nil, nil, RenameInfo{}, errors.New("edit projection: unsupported rename target")
		}
		info, ok := l.getSemanticRenameInfo(ctx, "", node, mapped.Script, program)
		if !ok || !info.CanRename {
			return nil, nil, RenameInfo{}, fmt.Errorf("edit projection: ineligible rename target: %s", info.LocalizedErrorMessage)
		}
		start, end := astnav.GetStartOfNode(node, mapped.Script, false), node.End()
		if ast.IsStringLiteralLike(node) {
			start++
			end--
		}
		rng := core.NewTextRange(start, end)
		trigger, fidelity := l.converters.ToLSPRange(mapped.Script, rng)
		if !fidelity.IsSingleSegment() {
			return nil, nil, RenameInfo{}, errors.New("edit projection: rename target crosses mapping boundaries")
		}
		if !fidelity.IsExact() {
			wholeToken := false
			for _, segment := range mapped.Script.SpanMap().Segments() {
				if int(segment.VirtualStart) == start && int(segment.VirtualEnd) == end && segment.OriginalStart < segment.OriginalEnd {
					wholeToken = true
					break
				}
			}
			if !wholeToken {
				return nil, nil, RenameInfo{}, errors.New("edit projection: atom is not a whole rename token")
			}
		}
		ch, done := program.GetTypeChecker(ctx)
		symbol := ch.GetSymbolAtLocation(node)
		done()
		if selectedNode != nil && (symbol == nil || symbol != selectedSymbol || selectedInfo.TriggerSpan != trigger) {
			return nil, nil, RenameInfo{}, errors.New("edit projection: ambiguous rename projections")
		}
		selectedNode, selectedFile, selectedSymbol = node, mapped.Script, symbol
		info.TriggerSpan = trigger
		selectedInfo = info
	}
	if selectedNode == nil {
		return nil, nil, RenameInfo{}, errors.New("edit projection: no rename target")
	}
	return selectedFile, selectedNode, selectedInfo, nil
}

// GetRenameEdits materializes generated edits across the orchestrator's programs before any projection.
// Callers must supply an orchestrator bound to one retained snapshot and validate the combined plan.
func (l *LanguageService) GetRenameEdits(ctx context.Context, params *lsproto.RenameParams, orchestrator CrossProjectOrchestrator) ([]editprojection.SourceEdit, error) {
	file, node, _, err := l.resolveProjectedRename(ctx, params.TextDocument.Uri, params.Position)
	if err != nil {
		return nil, err
	}
	position := astnav.GetStartOfNode(node, file, false)
	data := SymbolAndEntriesData{OriginalNode: node, Position: position, SymbolsAndEntries: l.getSymbolAndEntries(ctx, position, node, l.program, true, false)}
	return l.handleCrossProject(ctx, params, orchestrator, (*LanguageService).symbolAndEntriesToGeneratedEdits,
		func(results iter.Seq[[]editprojection.SourceEdit]) []editprojection.SourceEdit {
			var edits []editprojection.SourceEdit
			for batch := range results {
				edits = append(edits, batch...)
			}
			return edits
		}, true, false, symbolEntryTransformOptions{}, &data)
}

func (l *LanguageService) symbolAndEntriesToGeneratedEdits(ctx context.Context, params *lsproto.RenameParams, data SymbolAndEntriesData, _ symbolEntryTransformOptions) ([]editprojection.SourceEdit, error) {
	if data.OriginalNode == nil || len(data.SymbolsAndEntries) == 0 {
		return nil, nil
	}
	file := ast.GetSourceFileOfNode(data.OriginalNode)
	edits, err := l.materializeRenameEdits(ctx, file, data.OriginalNode, params.NewName, data.SymbolsAndEntries)
	if err != nil {
		return nil, err
	}
	if err := l.checkRenameProjectionCoverage(edits); err != nil {
		return nil, err
	}
	return edits, nil
}

// An authored token can feed multiple generated symbols. Updating it for just one of them would
// silently rename the others too. Require coverage even when the request starts in an ordinary .ts
// file, and inspect geometry without feature masks (a hidden projection is still affected by edits).
func (l *LanguageService) checkRenameProjectionCoverage(edits []editprojection.SourceEdit) error {
	type occurrence struct {
		file *ast.SourceFile
		rng  core.TextRange
	}
	covered := make(map[occurrence]bool, len(edits))
	originals := make(map[tspath.RootedFilePath]map[core.TextRange]bool)
	for _, edit := range edits {
		covered[occurrence{edit.File, edit.Change.TextRange}] = true
		if edit.File.SpanMap() == nil {
			continue
		}
		original, fidelity := edit.File.SpanMap().VirtualToOriginalSpan(edit.Change.TextRange)
		if fidelity.IsNone() {
			continue
		}
		if !fidelity.IsSingleSegment() {
			return errors.New("edit projection: rename occurrence crosses mapping boundaries")
		}
		name := edit.File.OriginalFileName()
		if originals[name] == nil {
			originals[name] = make(map[core.TextRange]bool)
		}
		originals[name][original] = true
	}
	for _, file := range l.program.GetSourceFiles() {
		ranges := originals[file.OriginalFileName()]
		if len(ranges) == 0 || file.SpanMap() == nil {
			continue
		}
		// Do not let presentation feature masks hide an affected projection. Intersections, rather
		// than whole-range lookups, also include projections of only part of the authored token.
		// The original map and its navigation fidelity are untouched.
		segments := slices.Clone(file.SpanMap().Segments())
		for i := range segments {
			segments[i].Features = spanmap.FeatureAll
		}
		geometry := spanmap.New(segments)
		for original := range ranges {
			for _, mapped := range geometry.OriginalToVirtualIntersectingSpans(original, spanmap.FeatureAll) {
				if !covered[occurrence{file, mapped.Span}] {
					return errors.New("edit projection: ambiguous or uncovered rename projection")
				}
			}
		}
	}
	return nil
}

func (l *LanguageService) materializeRenameEdits(ctx context.Context, file *ast.SourceFile, node *ast.Node, name string, symbols []*SymbolAndEntries) ([]editprojection.SourceEdit, error) {
	ch, done := l.program.GetTypeChecker(ctx)
	defer done()
	quote := lsutil.GetQuotePreference(file, l.UserPreferences())
	aliases := l.UserPreferences().ProvidePrefixAndSuffixTextForRename.IsTrueOrUnknown()
	var edits []editprojection.SourceEdit
	for _, symbol := range symbols {
		for _, entry := range symbol.references {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if l.UserPreferences().AllowRenameOfImportPath != core.TSTrue && entry.node != nil && ast.IsStringLiteralLike(entry.node) && ast.TryGetImportFromModuleSpecifier(entry.node) != nil {
				continue
			}
			l.resolveEntrySource(entry)
			if entry.sourceFile == nil || entry.textRange == nil {
				return nil, errors.New("edit projection: unresolved rename occurrence")
			}
			edits = append(edits, editprojection.SourceEdit{File: entry.sourceFile, Change: core.TextChange{
				TextRange: *entry.textRange,
				NewText:   l.getTextForRename(node, entry, name, ch, quote, aliases),
			}})
		}
	}
	if len(edits) == 0 {
		return nil, errors.New("edit projection: no rename occurrences")
	}
	return edits, nil
}

// GetOrganizeImportsEditPlan runs the existing import analysis but materializes generated-space edits
// before any authored mapping. Rendering and analysis finish before providers can be called.
func (l *LanguageService) GetOrganizeImportsEditPlan(ctx context.Context, uri lsproto.DocumentUri, kind lsproto.CodeActionKind, snapshot editprojection.Snapshot) (*editprojection.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kind != lsproto.CodeActionKindSourceOrganizeImportsTs && kind != lsproto.CodeActionKindSourceSortImportsTs && kind != lsproto.CodeActionKindSourceRemoveUnusedImportsTs {
		return nil, errors.New("edit projection: unsupported import action")
	}
	program, file := l.tryGetProgramAndFile(uri.FileName())
	if file == nil {
		return nil, errors.New("edit projection: source file not found")
	}
	changes, err := l.organizeImportsChanges(ctx, file, program, kind).GetGeneratedChanges()
	if err != nil {
		return nil, err
	}
	files := make([]*ast.SourceFile, 0, len(changes))
	for file := range changes {
		files = append(files, file)
	}
	slices.SortFunc(files, func(a, b *ast.SourceFile) int { return a.FileName().Compare(b.FileName()) })
	var edits []editprojection.SourceEdit
	for _, file := range files {
		for _, change := range changes[file] {
			edits = append(edits, editprojection.SourceEdit{File: file, Change: change})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return editprojection.NewPlan(snapshot, editprojection.Operation{Kind: editprojection.OrganizeImports, ImportAction: kind}, edits)
}
