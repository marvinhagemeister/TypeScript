package editprojection

import (
	"errors"
	"slices"

	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
)

type renameSpan struct {
	projection int
	span       core.TextRange
}

func projectionMapping(projection Projection) *spanmap.SpanMap {
	if !projection.Mapped {
		return nil
	}
	return spanmap.New(projection.Mappings)
}

// Enumerate actual geometry, never presentation feature masks. Include both sides of an insertion
// boundary: an insertion must not slip between adjacent or zero-width projections unnoticed.
func affectedSpans(projection Projection, original core.TextRange) []core.TextRange {
	if !projection.Mapped {
		return []core.TextRange{original}
	}
	var result []core.TextRange
	for _, segment := range projection.Mappings {
		start, end := max(original.Pos(), int(segment.OriginalStart)), min(original.End(), int(segment.OriginalEnd))
		if original.Pos() == original.End() {
			if start > end {
				continue
			}
		} else if segment.OriginalStart == segment.OriginalEnd {
			if int(segment.OriginalStart) < original.Pos() || int(segment.OriginalStart) >= original.End() {
				continue
			}
		} else if start >= end {
			continue
		}
		if segment.Kind == spanmap.KindVerbatim {
			result = append(result, core.NewTextRange(int(segment.VirtualStart)+start-int(segment.OriginalStart), int(segment.VirtualStart)+end-int(segment.OriginalStart)))
		} else {
			result = append(result, core.NewTextRange(int(segment.VirtualStart), int(segment.VirtualEnd)))
		}
	}
	return result
}

func (p *Plan) collectRenameEffects() error {
	covered := make(map[renameSpan]bool)
	for _, edit := range p.request.Edits {
		covered[renameSpan{edit.Projection, edit.Change.TextRange}] = true
	}
	effects := make(map[renameSpan]int)
	for _, edit := range p.request.Edits {
		source := p.request.Projections[edit.Projection]
		original, fidelity := projectionMapping(source).VirtualToOriginalSpan(edit.Change.TextRange)
		if fidelity.IsNone() {
			continue
		}
		if !fidelity.IsSingleSegment() {
			return errors.New("edit projection: rename occurrence crosses mapping boundaries")
		}
		for _, projection := range p.request.Projections {
			if projection.Document != source.Document {
				continue
			}
			for _, span := range affectedSpans(projection, original) {
				key := renameSpan{projection.ID, span}
				if covered[key] {
					continue
				}
				if !projection.DerivedRename {
					return errors.New("edit projection: ambiguous or uncovered rename projection")
				}
				id, exists := effects[key]
				if !exists {
					id = len(p.request.Effects)
					effects[key] = id
					p.request.Effects = append(p.request.Effects, RenameEffect{ID: id, Projection: projection.ID, Span: span})
				}
				if !slices.Contains(p.request.Effects[id].Inputs, edit.ID) {
					p.request.Effects[id].Inputs = append(p.request.Effects[id].Inputs, edit.ID)
				}
			}
		}
	}
	return nil
}

// This is structural accounting, not proof that the mapper's claimed derivation is semantically
// correct. The mapper owns language semantics; the host owns the frozen write footprint.
// validateResponse has already checked input coverage, result structure and all authored ranges.
func (p *Plan) validateRenameResponse(owner string, batch []GeneratedEdit, response Response) error {
	enabled := false
	for _, edit := range batch {
		enabled = enabled || p.request.Projections[edit.Projection].DerivedRename
	}
	if !enabled {
		for _, result := range response.Results {
			if len(result.DerivedEffects) != 0 {
				return errors.New("edit projection: derived rename was not negotiated")
			}
		}
		return nil
	}
	covered := make(map[renameSpan]bool)
	for _, edit := range batch {
		covered[renameSpan{edit.Projection, edit.Change.TextRange}] = true
	}
	remaining := make(map[int]RenameEffect)
	for _, effect := range p.request.Effects {
		if p.request.Documents[p.request.Projections[effect.Projection].Document].Owner == owner {
			remaining[effect.ID] = effect
		}
	}
	for _, result := range response.Results {
		for _, id := range result.DerivedEffects {
			effect, ok := remaining[id]
			if !ok {
				return errors.New("edit projection: unknown or duplicate derived effect")
			}
			rooted := false
			for _, input := range result.Inputs {
				rooted = rooted || slices.Contains(effect.Inputs, input)
			}
			projection := p.request.Projections[effect.Projection]
			touched := false
			for _, edit := range result.Edits {
				if edit.Document == projection.Document {
					touched = touched || slices.Contains(affectedSpans(projection, edit.Change.TextRange), effect.Span)
				}
			}
			if !rooted || !touched {
				return errors.New("edit projection: derived effect is not rooted in the result")
			}
			covered[renameSpan{effect.Projection, effect.Span}] = true
			delete(remaining, id)
		}
	}
	if len(remaining) != 0 {
		return errors.New("edit projection: incomplete derived effect coverage")
	}
	// Widened and supplemental edits must not touch new effects outside the frozen request. There is
	// no retry or document-wide waiver: an expansion that cannot be accounted for fails atomically.
	for _, result := range response.Results {
		for _, edit := range result.Edits {
			for _, projection := range p.request.Projections {
				if projection.Document != edit.Document {
					continue
				}
				for _, span := range affectedSpans(projection, edit.Change.TextRange) {
					if !covered[renameSpan{projection.ID, span}] {
						return errors.New("edit projection: unaccounted authored edit impact")
					}
				}
			}
		}
	}
	return nil
}
