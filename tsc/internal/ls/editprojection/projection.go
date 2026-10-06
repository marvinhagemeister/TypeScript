// Package editprojection prototypes the boundary between generated semantic edits and authored edits.
// It is an internal model; the experimental mapper RPC adapter is separate. See README.md.
package editprojection

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/ls/lsconv"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
)

var ErrStaleSnapshot = errors.New("edit projection: stale snapshot")

type Kind string

const (
	Rename          Kind = "rename"
	OrganizeImports Kind = "organizeImports"
)

type Operation struct {
	Kind         Kind
	NewName      string // Canonical semantic name, not a per-occurrence replacement expression.
	ImportAction lsproto.CodeActionKind
}

// Snapshot.ID must change whenever any program input or mapper configuration changes, including
// closed documents. Versions contains open authored documents only; missing entries serialize as null.
type Snapshot struct {
	ID       string
	Versions map[string]int32
}

type SourceEdit struct {
	File   *ast.SourceFile
	Change core.TextChange
	// Context distinguishes programs even when their cached SourceFile objects are shared.
	Context string
}

// SourceProjection includes edit-free generated views of an authorized authored document.
// This is the authority for provider ownership and capabilities. Callers must supply every view in
// the frozen operation's projects; only documents containing native edits are authorized.
type SourceProjection struct {
	File          *ast.SourceFile
	Owner         string
	Context       string
	DerivedRename bool
}

type Document struct {
	ID       int
	FileName tspath.RootedFilePath
	Text     string
	Owner    string
	Version  *int32
}

type Projection struct {
	ID                int
	Document          int
	FileName          tspath.RootedFilePath
	Text              string
	TransformIdentity string
	Mappings          []spanmap.Segment
	Mapped            bool
	DerivedRename     bool
}

type GeneratedEdit struct {
	ID         int
	Projection int
	Change     core.TextChange
}

// Request contains only value data. Providers receive private copies of slices and versions, not ASTs,
// checkers, or the mutable state used by validation. IDs remain stable in provider-specific batches.
type Request struct {
	Snapshot    string
	Operation   Operation
	Documents   []Document
	Projections []Projection
	Edits       []GeneratedEdit
	Effects     []RenameEffect
}

type AuthoredEdit struct {
	Document int
	Change   core.TextChange
}

// RenameEffect is an affected generated span outside the native rename set. It is not an edit.
// Inputs identifies semantic edits whose authored origins cause this effect.
type RenameEffect struct {
	ID         int
	Projection int
	Span       core.TextRange
	Inputs     []int
}

type Result struct {
	DerivedEffects []int
	Inputs         []int
	Edits          []AuthoredEdit
	GeneratedOnly  bool
	Reason         string
}

type Response struct {
	Snapshot string
	Results  []Result
}

type Provider func(context.Context, Request) (Response, error)

// Plan holds materialized edits, with no checker resource retained across provider calls.
type Plan struct {
	request Request
}

func NewPlan(snapshot Snapshot, operation Operation, edits []SourceEdit, views ...SourceProjection) (*Plan, error) {
	if snapshot.ID == "" || operation.Kind != Rename && operation.Kind != OrganizeImports {
		return nil, errors.New("edit projection: invalid snapshot or operation")
	}
	plan := &Plan{request: Request{Snapshot: snapshot.ID, Operation: operation}}
	documents := make(map[tspath.RootedFilePath]int)
	type viewKey struct {
		file    *ast.SourceFile
		context string
	}
	projections := make(map[viewKey]int)
	identities := make(map[tspath.RootedFilePath]string)
	catalogue := make(map[viewKey]SourceProjection)
	for _, view := range views {
		key := viewKey{view.File, view.Context}
		if previous, exists := catalogue[key]; exists && previous != view {
			return nil, errors.New("edit projection: inconsistent projection context")
		}
		catalogue[key] = view
	}
	register := func(key viewKey) (int, error) {
		file := key.file
		if file == nil || file.IsContentMapperFailureStub() {
			return 0, errors.New("edit projection: invalid generated projection")
		}
		if id, exists := projections[key]; exists {
			return id, nil
		}
		// Metadata comes only from the catalogue; undeclared views retain the strict defaults.
		view := catalogue[key]
		owner := view.Owner
		if owner == "" {
			owner = file.ContentMapper()
		}
		name := file.OriginalFileName()
		documentID, exists := documents[name]
		if exists {
			document := plan.request.Documents[documentID]
			if document.Text != file.OriginalText() || document.Owner != owner || identities[name] != file.ContentMapperTransformIdentity() {
				return 0, fmt.Errorf("edit projection: inconsistent projections for %s", name)
			}
		} else {
			documentID = len(plan.request.Documents)
			document := Document{ID: documentID, FileName: name, Text: file.OriginalText(), Owner: owner}
			if version, ok := snapshot.Versions[name.AsString()]; ok {
				document.Version = new(version)
			}
			plan.request.Documents = append(plan.request.Documents, document)
			documents[name] = documentID
			identities[name] = file.ContentMapperTransformIdentity()
		}
		projectionID := len(plan.request.Projections)
		projection := Projection{ID: projectionID, Document: documentID, FileName: file.FileName(), Text: file.Text(), TransformIdentity: file.ContentMapperTransformIdentity(), DerivedRename: view.DerivedRename}
		if mapping := file.SpanMap(); mapping != nil {
			if err := mapping.Validate(file.Text(), file.OriginalText()); err != nil {
				return 0, err
			}
			projection.Mapped = true
			projection.Mappings = slices.Clone(mapping.Segments())
		}
		plan.request.Projections = append(plan.request.Projections, projection)
		projections[key] = projectionID
		return projectionID, nil
	}
	for _, edit := range edits {
		if edit.File == nil || !validRange(edit.File.Text(), edit.Change.TextRange) {
			return nil, errors.New("edit projection: invalid generated edit")
		}
		id, err := register(viewKey{edit.File, edit.Context})
		if err != nil {
			return nil, err
		}
		plan.request.Edits = append(plan.request.Edits, GeneratedEdit{ID: len(plan.request.Edits), Projection: id, Change: edit.Change})
	}
	for _, view := range views {
		if view.File == nil {
			return nil, errors.New("edit projection: invalid generated projection")
		}
		if _, authorized := documents[view.File.OriginalFileName()]; !authorized {
			continue
		}
		if _, err := register(viewKey{view.File, view.Context}); err != nil {
			return nil, err
		}
	}
	if operation.Kind == Rename {
		if err := plan.collectRenameEffects(); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// Project produces a complete workspace edit or an error, never a partial workspace edit. Providers
// must respect ctx (including any host deadline). currentSnapshot reads the host's current revision;
// this check does not replace client-side version validation when the workspace edit is applied.
func (p *Plan) Project(ctx context.Context, providers map[string]Provider, currentSnapshot func() string, encoding lsproto.PositionEncodingKind) (*lsproto.WorkspaceEdit, error) {
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if currentSnapshot == nil || currentSnapshot() != p.request.Snapshot {
			return ErrStaleSnapshot
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, err
	}
	if encoding != lsproto.PositionEncodingKindUTF8 && encoding != lsproto.PositionEncodingKindUTF16 {
		return nil, errors.New("edit projection: unsupported position encoding")
	}
	for _, effect := range p.request.Effects {
		owner := p.request.Documents[p.request.Projections[effect.Projection].Document].Owner
		if owner == "" || providers[owner] == nil {
			return nil, errors.New("edit projection: no provider for required derived effect")
		}
	}
	batches := make(map[string][]GeneratedEdit)
	var authored []AuthoredEdit
	for _, edit := range p.request.Edits {
		projection := p.request.Projections[edit.Projection]
		document := p.request.Documents[projection.Document]
		if document.Owner != "" && providers[document.Owner] != nil {
			batches[document.Owner] = append(batches[document.Owner], edit)
			continue
		}
		var mapping *spanmap.SpanMap
		if projection.Mapped {
			mapping = spanmap.New(projection.Mappings)
		}
		rng, fidelity := mapping.VirtualToOriginalSpan(edit.Change.TextRange)
		if edit.Change.Pos() == edit.Change.End() {
			if _, exact := mapping.VirtualToOriginalPositionExact(core.TextPos(edit.Change.Pos())); !exact {
				fidelity = spanmap.FidelityNone
			}
		}
		if !fidelity.IsExact() {
			return nil, fmt.Errorf("edit projection: no provider for required edit in %s", document.FileName)
		}
		authored = append(authored, AuthoredEdit{Document: document.ID, Change: core.TextChange{TextRange: rng, NewText: edit.Change.NewText}})
	}
	owners := make([]string, 0, len(batches))
	for owner := range batches {
		owners = append(owners, owner)
	}
	slices.Sort(owners)
	for _, owner := range owners {
		if err := check(); err != nil {
			return nil, err
		}
		batch := batches[owner]
		response, providerErr := providers[owner](ctx, p.batch(owner, batch))
		if providerErr != nil {
			return nil, fmt.Errorf("edit projection: provider %s: %w", owner, providerErr)
		}
		if err := check(); err != nil {
			return nil, err
		}
		edits, err := p.validateResponse(owner, batch, response)
		if err != nil {
			return nil, err
		}
		authored = append(authored, edits...)
	}
	result, err := p.workspaceEdit(authored, encoding)
	if err != nil {
		return nil, err
	}
	if err := check(); err != nil {
		return nil, err
	}
	return result, nil
}

func (p *Plan) batch(owner string, edits []GeneratedEdit) Request {
	request := Request{Snapshot: p.request.Snapshot, Operation: p.request.Operation, Edits: slices.Clone(edits)}
	for _, document := range p.request.Documents {
		if document.Owner == owner {
			if document.Version != nil {
				document.Version = new(*document.Version)
			}
			request.Documents = append(request.Documents, document)
		}
	}
	for _, projection := range p.request.Projections {
		if p.request.Documents[projection.Document].Owner == owner {
			projection.Mappings = slices.Clone(projection.Mappings)
			request.Projections = append(request.Projections, projection)
		}
	}
	for _, effect := range p.request.Effects {
		if p.request.Documents[p.request.Projections[effect.Projection].Document].Owner == owner {
			effect.Inputs = slices.Clone(effect.Inputs)
			request.Effects = append(request.Effects, effect)
		}
	}
	return request
}

func (p *Plan) validateResponse(owner string, batch []GeneratedEdit, response Response) ([]AuthoredEdit, error) {
	if response.Snapshot != p.request.Snapshot {
		return nil, errors.New("edit projection: provider returned stale snapshot")
	}
	remaining := make(map[int]GeneratedEdit, len(batch))
	for _, edit := range batch {
		remaining[edit.ID] = edit
	}
	var edits []AuthoredEdit
	for _, result := range response.Results {
		if len(result.Inputs) == 0 ||
			result.GeneratedOnly && (len(result.Edits) != 0 || len(result.DerivedEffects) != 0 || result.Reason == "") ||
			!result.GeneratedOnly && len(result.Edits) == 0 ||
			p.request.Operation.Kind != Rename && len(result.DerivedEffects) != 0 {
			return nil, errors.New("edit projection: invalid coverage result")
		}
		for _, id := range result.Inputs {
			input, ok := remaining[id]
			if !ok {
				return nil, fmt.Errorf("edit projection: unknown or duplicate input %d", id)
			}
			if result.GeneratedOnly && hasAuthoredOrigin(p.request.Projections[input.Projection], input.Change.TextRange) {
				return nil, errors.New("edit projection: cannot discard an authored occurrence")
			}
			delete(remaining, id)
		}
		for _, edit := range result.Edits {
			if edit.Document < 0 || edit.Document >= len(p.request.Documents) || p.request.Documents[edit.Document].Owner != owner {
				return nil, errors.New("edit projection: unauthorized authored document")
			}
			if !validRange(p.request.Documents[edit.Document].Text, edit.Change.TextRange) {
				return nil, errors.New("edit projection: invalid authored range")
			}
			edits = append(edits, edit)
		}
	}
	if len(remaining) != 0 {
		return nil, errors.New("edit projection: incomplete coverage")
	}
	if p.request.Operation.Kind == Rename {
		if err := p.validateRenameResponse(owner, batch, response); err != nil {
			return nil, err
		}
	}
	return edits, nil
}

func hasAuthoredOrigin(projection Projection, rng core.TextRange) bool {
	if !projection.Mapped {
		return true
	}
	for _, segment := range projection.Mappings {
		if rng.Pos() == rng.End() {
			if int(segment.VirtualStart) <= rng.Pos() && rng.Pos() <= int(segment.VirtualEnd) {
				return true
			}
		} else if int(segment.VirtualStart) < rng.End() && rng.Pos() < int(segment.VirtualEnd) {
			return true
		}
	}
	return false
}

func validRange(text string, rng core.TextRange) bool {
	validBoundary := func(pos int) bool {
		return pos >= 0 && pos <= len(text) && (pos == len(text) || utf8.RuneStart(text[pos])) &&
			!(pos > 0 && pos < len(text) && text[pos-1] == '\r' && text[pos] == '\n')
	}
	return rng.Pos() <= rng.End() && validBoundary(rng.Pos()) && validBoundary(rng.End())
}

func (p *Plan) workspaceEdit(edits []AuthoredEdit, encoding lsproto.PositionEncodingKind) (*lsproto.WorkspaceEdit, error) {
	slices.SortFunc(edits, func(a, b AuthoredEdit) int {
		if a.Document != b.Document {
			return a.Document - b.Document
		}
		if a.Change.Pos() != b.Change.Pos() {
			return a.Change.Pos() - b.Change.Pos()
		}
		return a.Change.End() - b.Change.End()
	})
	var unique []AuthoredEdit
	for _, edit := range edits {
		if !validRange(p.request.Documents[edit.Document].Text, edit.Change.TextRange) {
			return nil, errors.New("edit projection: invalid authored range")
		}
		if len(unique) != 0 {
			previous := unique[len(unique)-1]
			if previous.Document == edit.Document {
				if previous.Change == edit.Change {
					continue
				}
				if previous.Change.End() > edit.Change.Pos() || previous.Change.Pos() == edit.Change.Pos() {
					return nil, errors.New("edit projection: conflicting authored edits")
				}
			}
		}
		unique = append(unique, edit)
	}
	unique = slices.DeleteFunc(unique, func(edit AuthoredEdit) bool {
		return p.request.Documents[edit.Document].Text[edit.Change.Pos():edit.Change.End()] == edit.Change.NewText
	})
	var changes []lsproto.TextDocumentEditOrCreateFileOrRenameFileOrDeleteFile
	next := 0
	for _, document := range p.request.Documents {
		if next == len(unique) || unique[next].Document != document.ID {
			continue
		}
		var textEdits []lsproto.TextEditOrAnnotatedTextEditOrSnippetTextEdit
		lineMap := lsconv.ComputeLSPLineStarts(document.Text)
		converters := lsconv.NewConverters(encoding, func(tspath.RootedFilePath) *lsconv.LSPLineMap { return lineMap })
		for next < len(unique) && unique[next].Document == document.ID {
			edit := unique[next]
			next++
			rng, _ := converters.ToLSPRange(authoredScript{document}, edit.Change.TextRange)
			textEdits = append(textEdits, lsproto.TextEditOrAnnotatedTextEditOrSnippetTextEdit{TextEdit: &lsproto.TextEdit{Range: rng, NewText: edit.Change.NewText}})
		}
		if document.Version != nil {
			document.Version = new(*document.Version)
		}
		if len(textEdits) != 0 {
			changes = append(changes, lsproto.TextDocumentEditOrCreateFileOrRenameFileOrDeleteFile{TextDocumentEdit: &lsproto.TextDocumentEdit{
				TextDocument: lsproto.OptionalVersionedTextDocumentIdentifier{Uri: lsconv.FileNameToDocumentURI(document.FileName), Version: lsproto.IntegerOrNull{Integer: document.Version}},
				Edits:        textEdits,
			}})
		}
	}
	return &lsproto.WorkspaceEdit{DocumentChanges: &changes}, nil
}

type authoredScript struct{ document Document }

func (s authoredScript) FileName() tspath.RootedFilePath         { return s.document.FileName }
func (s authoredScript) OriginalFileName() tspath.RootedFilePath { return s.document.FileName }
func (s authoredScript) Text() string                            { return s.document.Text }
func (s authoredScript) OriginalText() string                    { return s.document.Text }
func (authoredScript) SpanMap() *spanmap.SpanMap                 { return nil }
