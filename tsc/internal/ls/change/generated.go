package change

import (
	"errors"
	"slices"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
)

// GetGeneratedChanges materializes edits in each generated source's UTF-8 byte coordinates, without
// authored conversion, authored indentation, or filtering. As with GetChanges, the tracker is consumed.
// This is the experimental edit-projection boundary, not an alternative safety policy for GetChanges.
func (t *Tracker) GetGeneratedChanges() (map[*ast.SourceFile][]core.TextChange, error) {
	t.finishDeleteDeclarations()
	t.finishNodesWithInsertionsAtStart()
	if t.unmappableFiles.Len() != 0 {
		return nil, errors.New("cannot materialize generated edits after an unmappable authored input")
	}
	result := make(map[*ast.SourceFile][]core.TextChange)
	for file, changes := range t.changes.M {
		for _, edit := range changes {
			result[file] = append(result[file], core.TextChange{
				TextRange: edit.TextRange,
				NewText:   t.computeNewTextAt(edit, file, file, edit.TextRange.Pos()),
			})
		}
		slices.SortStableFunc(result[file], func(a, b core.TextChange) int {
			if a.Pos() != b.Pos() {
				return a.Pos() - b.Pos()
			}
			return a.End() - b.End()
		})
	}
	return result, nil
}
