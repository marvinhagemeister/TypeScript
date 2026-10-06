package lsp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strconv"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/collections"
	"github.com/microsoft/TypeScript/tsc/internal/compiler"
	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/json"
	"github.com/microsoft/TypeScript/tsc/internal/ls"
	"github.com/microsoft/TypeScript/tsc/internal/ls/editprojection"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/project"
	"github.com/microsoft/TypeScript/tsc/internal/scanner"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
)

func mapperEditError(err error) error {
	if errors.Is(err, editprojection.ErrStaleSnapshot) {
		return lsproto.ErrorCodeContentModified
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, lsproto.ErrorCodeContentModified) {
		return err
	}
	return userFacingRequestFailedError(err.Error())
}

type mapperEditRoute struct {
	project  contentmapper.Project
	editor   contentmapper.EditingProject
	mapper   *contentmapper.Mapper
	identity string
}

func editRoute(program *compiler.Program, file *ast.SourceFile, kind editprojection.Kind) *mapperEditRoute {
	if file == nil || file.ContentMapper() == "" {
		return nil
	}
	mapperProject := program.ContentMapperProject()
	editor, ok := mapperProject.(contentmapper.EditingProject)
	if !ok {
		return nil
	}
	for _, mapper := range program.CommandLine().ContentMappers() {
		if mapper.Identity() != file.ContentMapper() {
			continue
		}
		caps := editor.EditCapabilities(mapper)
		if caps.Version == contentmapper.EditProjectionVersion && (kind == editprojection.Rename && caps.Rename || kind == editprojection.OrganizeImports && caps.OrganizeImports) {
			return &mapperEditRoute{mapperProject, editor, mapper, file.ContentMapperTransformIdentity()}
		}
	}
	return nil
}

func (r *mapperEditRoute) owner(projectContext string) string {
	return fmt.Sprintf("%q:%q:%q", projectContext, r.mapper.Identity(), r.identity)
}

func (r *mapperEditRoute) checkIdentity() error {
	identity, err := r.project.Identity(r.mapper)
	if err != nil {
		return err
	}
	if identity != r.identity {
		return lsproto.ErrorCodeContentModified
	}
	return nil
}

func (r *mapperEditRoute) provider(ctx context.Context, request editprojection.Request) (editprojection.Response, error) {
	if err := r.checkIdentity(); err != nil {
		return editprojection.Response{}, err
	}
	params := contentmapper.ProjectEditsParams{Snapshot: request.Snapshot, Operation: string(request.Operation.Kind), NewName: request.Operation.NewName, ImportAction: string(request.Operation.ImportAction)}
	documentLengths := make(map[int]int, len(request.Documents))
	for _, doc := range request.Documents {
		documentLengths[doc.ID] = len(doc.Text)
		params.Documents = append(params.Documents, contentmapper.EditDocument{ID: doc.ID, FileName: doc.FileName.AsString(), Text: doc.Text, Version: doc.Version})
	}
	for _, projection := range request.Projections {
		if projection.TransformIdentity != r.identity {
			return editprojection.Response{}, lsproto.ErrorCodeContentModified
		}
		mappings, err := spanmap.New(projection.Mappings).Marshal()
		if err != nil {
			return editprojection.Response{}, err
		}
		params.Projections = append(params.Projections, contentmapper.EditProjection{ID: projection.ID, Document: projection.Document, FileName: projection.FileName.AsString(), Text: projection.Text, TransformIdentity: projection.TransformIdentity, Mappings: json.Value(mappings)})
	}
	for _, edit := range request.Edits {
		params.Edits = append(params.Edits, contentmapper.GeneratedEdit{ID: edit.ID, Projection: edit.Projection, Start: edit.Change.Pos(), End: edit.Change.End(), NewText: edit.Change.NewText})
	}
	for _, effect := range request.Effects {
		params.Effects = append(params.Effects, contentmapper.RenameEffect{ID: effect.ID, Projection: effect.Projection, Start: effect.Span.Pos(), End: effect.Span.End(), Inputs: effect.Inputs})
	}
	result, err := r.editor.ProjectEdits(ctx, r.mapper, params)
	if err != nil {
		return editprojection.Response{}, err
	}
	if err := r.checkIdentity(); err != nil {
		return editprojection.Response{}, err
	}
	response := editprojection.Response{Snapshot: result.Snapshot}
	for _, result := range result.Results {
		coverage := editprojection.Result{Inputs: result.Inputs, GeneratedOnly: result.GeneratedOnly, Reason: result.Reason, DerivedEffects: result.DerivedEffects}
		for _, edit := range result.Edits {
			// Validate wire integers before narrowing them to core.TextPos (int32).
			length, authorized := documentLengths[edit.Document]
			if !authorized || edit.Start < 0 || edit.End < edit.Start || edit.End > length {
				return editprojection.Response{}, errors.New("content mapper returned an unauthorized document or invalid edit range")
			}
			coverage.Edits = append(coverage.Edits, editprojection.AuthoredEdit{Document: edit.Document, Change: core.TextChange{TextRange: core.NewTextRange(edit.Start, edit.End), NewText: edit.NewText}})
		}
		response.Results = append(response.Results, coverage)
	}
	return response, nil
}

func editSnapshot(snapshot *project.Snapshot, programs []*compiler.Program) editprojection.Snapshot {
	result := editprojection.Snapshot{ID: strconv.FormatUint(snapshot.ID(), 10), Versions: make(map[string]int32)}
	for _, program := range programs {
		for _, source := range program.GetSourceFiles() {
			if file := snapshot.GetFile(source.OriginalFileName()); file != nil && file.IsOverlay() {
				result.Versions[source.OriginalFileName().AsString()] = file.Version()
			}
		}
	}
	return result
}

// Flushing pending notifications matters: Snapshot().ID() alone can still be unchanged immediately
// after didChange. The retained input snapshot remains alive while a new current snapshot is acquired.
func (s *Server) editFreshness(ctx context.Context, uri lsproto.DocumentUri, snapshot *project.Snapshot, sources []*ast.SourceFile) func() string {
	// A file can contribute thousands of references or several projections; read each destination
	// once per freshness check, not once per generated edit.
	expected := make(map[tspath.RootedFilePath]string)
	for _, source := range sources {
		expected[source.OriginalFileName()] = source.OriginalText()
	}
	return func() string {
		if ctx.Err() != nil {
			return ""
		}
		var id string
		s.session.WithSnapshotForDocument(ctx, uri, func(current *project.Snapshot) {
			if current.ID() != snapshot.ID() {
				return
			}
			for name, original := range expected {
				file := current.GetFile(name)
				if file == nil || file.Content() != original {
					return
				}
				if !file.IsOverlay() {
					text, ok := s.fs.ReadFile(file.FileName())
					if !ok || text != file.Content() {
						return
					}
				}
			}
			id = strconv.FormatUint(current.ID(), 10)
		})
		return id
	}
}

func validProjectedRenameName(name string) bool {
	return scanner.IsValidIdentifier(name) && scanner.GetIdentifierToken(name) == ast.KindIdentifier
}

// newName is nil for preparation and the untouched user input for execution. Only the initiating
// mapper normalizes it; destination providers and TypeScript receive the canonical name.
func (s *Server) prepareMapperRename(ctx context.Context, snapshot *project.Snapshot, service *ls.LanguageService, uri lsproto.DocumentUri, position lsproto.Position, route *mapperEditRoute, newName *string) (ls.RenameInfo, string, error) {
	info, err := service.PrepareProjectedRename(ctx, uri, position)
	if err != nil {
		return ls.RenameInfo{}, "", err
	}
	file := service.GetProgram().GetSourceFile(uri.FileName())
	spans := snapshot.Converters().FromLSPRange(originalEditScript{file}, info.TriggerSpan, spanmap.FeatureAll)
	if len(spans) != 1 {
		return ls.RenameInfo{}, "", errors.New("rename trigger has no authored range")
	}
	if identityErr := route.checkIdentity(); identityErr != nil {
		return ls.RenameInfo{}, "", identityErr
	}
	renameInput := route.editor.EditCapabilities(route.mapper).RenameInput
	params := contentmapper.PrepareRenameParams{
		Snapshot: strconv.FormatUint(snapshot.ID(), 10), FileName: file.OriginalFileName().AsString(), Content: file.OriginalText(),
		Start: spans[0].Span.Pos(), End: spans[0].Span.End(), Name: info.DisplayName,
	}
	if renameInput {
		params.NewName = newName
	}
	result, err := route.editor.PrepareRename(ctx, route.mapper, params)
	if err != nil {
		return ls.RenameInfo{}, "", err
	}
	if err := ctx.Err(); err != nil {
		return ls.RenameInfo{}, "", err
	}
	if err := route.checkIdentity(); err != nil {
		return ls.RenameInfo{}, "", err
	}
	if s.editFreshness(ctx, uri, snapshot, []*ast.SourceFile{file})() != params.Snapshot {
		return ls.RenameInfo{}, "", lsproto.ErrorCodeContentModified
	}
	if !result.CanRename {
		return ls.RenameInfo{}, "", fmt.Errorf("content mapper cannot rename this element: %s", result.Message)
	}
	canonicalName := ""
	if newName != nil {
		canonicalName = *newName
	}
	if renameInput {
		if result.Placeholder == nil || *result.Placeholder == "" {
			return ls.RenameInfo{}, "", errors.New("content mapper returned a missing or empty authored rename placeholder")
		}
		info.DisplayName = *result.Placeholder
		if newName != nil {
			if result.NormalizedName == nil || !validProjectedRenameName(*result.NormalizedName) {
				return ls.RenameInfo{}, "", errors.New("content mapper returned an invalid or missing normalized rename name")
			}
			canonicalName = *result.NormalizedName
		}
	}
	return info, canonicalName, nil
}

func (s *Server) tryPrepareMapperRename(ctx context.Context, service *ls.LanguageService, params *lsproto.PrepareRenameParams) (info ls.RenameInfo, handled bool, err error) {
	route := editRoute(service.GetProgram(), service.GetProgram().GetSourceFile(params.TextDocument.Uri.FileName()), editprojection.Rename)
	if route == nil {
		return
	}
	handled = true
	s.session.WithSnapshotForDocument(ctx, params.TextDocument.Uri, func(snapshot *project.Snapshot) {
		proj := snapshot.GetDefaultProject(params.TextDocument.Uri)
		if proj == nil || proj.GetProgram() != service.GetProgram() {
			err = lsproto.ErrorCodeContentModified
			return
		}
		info, _, err = s.prepareMapperRename(ctx, snapshot, service, params.TextDocument.Uri, params.Position, route, nil)
	})
	return
}

// frozenEditProjects reuses native cross-project symbol traversal, but never switches snapshots or
// independently projects a project's edits. The caller has already loaded and retained the tree.
type frozenEditProjects struct {
	snapshot *project.Snapshot
	primary  *project.Project
	uri      lsproto.DocumentUri
}

func (p *frozenEditProjects) GetDefaultProject() ls.Project { return p.primary }
func (p *frozenEditProjects) GetAllProjectsForInitialRequest() []ls.Project {
	return p.snapshot.GetLanguageServiceProjectsContainingFile(p.uri)
}

func (p *frozenEditProjects) GetLanguageServiceForProjectWithFile(_ context.Context, proj ls.Project, uri lsproto.DocumentUri) *ls.LanguageService {
	if !proj.HasFile(uri.FileName()) {
		return nil
	}
	concrete := proj.(*project.Project)
	return ls.NewLanguageService(concrete.ID(), concrete.GetProgram(), p.snapshot, uri.FileName().AsString())
}

func (p *frozenEditProjects) GetProjectsForFile(_ context.Context, uri lsproto.DocumentUri) ([]ls.Project, error) {
	return p.snapshot.GetLanguageServiceProjectsContainingFile(uri), nil
}

func (p *frozenEditProjects) GetProjectsLoadingProjectTree(_ context.Context, _ *collections.Set[tspath.PathKey]) iter.Seq[ls.Project] {
	return func(yield func(ls.Project) bool) {
		for _, proj := range p.snapshot.ProjectCollection.LanguageServiceProjects() {
			if !yield(proj) {
				return
			}
		}
	}
}

// Capture and retain the request's project tree synchronously, then run all mapper callbacks outside
// the dispatcher. Otherwise a slow mapper would block didChange and prevent freshness validation.
func (s *Server) handleRenameRequest(ctx context.Context, req *lsproto.RequestMessage) (func() error, error) {
	if s.session == nil {
		return nil, lsproto.ErrorCodeServerNotInitialized
	}
	params, err := req.UnmarshalParams[*lsproto.RenameParams]()
	if err != nil {
		return nil, err
	}
	if work := s.mapperRenameWork(ctx, params); work != nil {
		return func() error {
			defer s.recover(req)
			response, workErr := work()
			if workErr != nil {
				return mapperEditError(workErr)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return s.sendResult(req.ID, response)
		}, nil
	}
	response, err := s.handleRename(ctx, params, req)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, s.sendResult(req.ID, response)
}

func (s *Server) mapperRenameWork(ctx context.Context, params *lsproto.RenameParams) (work func() (lsproto.RenameResponse, error)) {
	var hasMappers bool
	s.session.WithSnapshotForDocument(ctx, params.TextDocument.Uri, func(snapshot *project.Snapshot) { hasMappers = len(snapshot.ContentMapperExtensions()) != 0 })
	if !hasMappers {
		return nil
	}
	s.session.WithSnapshotLoadingProjectTree(ctx, nil, func(snapshot *project.Snapshot) {
		primary := snapshot.GetDefaultProject(params.TextDocument.Uri)
		if primary == nil {
			return
		}
		var programs []*compiler.Program
		type routeKey struct {
			file    *ast.SourceFile
			context string
		}
		routes := make(map[routeKey]*mapperEditRoute)
		var views []editprojection.SourceProjection
		for _, proj := range snapshot.ProjectCollection.LanguageServiceProjects() {
			program := proj.GetProgram()
			if program == nil {
				continue
			}
			programs = append(programs, program)
			for _, file := range program.GetSourceFiles() {
				view := editprojection.SourceProjection{File: file, Context: proj.ID().String()}
				if route := editRoute(program, file, editprojection.Rename); route != nil {
					routes[routeKey{file, view.Context}] = route
					// Context, not just transform identity, scopes mapper editing state.
					view.Owner = route.owner(view.Context)
					view.DerivedRename = route.editor.EditCapabilities(route.mapper).DerivedRename
				}
				views = append(views, view)
			}
		}
		if len(routes) == 0 {
			return
		}
		service := ls.NewLanguageService(primary.ID(), primary.GetProgram(), snapshot, params.TextDocument.Uri.FileName().AsString())
		if info := service.GetRenameInfo(ctx, params.NewName, params.TextDocument.Uri, params.Position); info.FileToRename != "" {
			return
		}
		release := snapshot.Retain()
		work = func() (response lsproto.RenameResponse, err error) {
			defer release()
			// Keep the original LSP request immutable, including for any other consumers of it.
			semanticParams := *params
			origin := primary.GetProgram().GetSourceFile(params.TextDocument.Uri.FileName())
			if route := routes[routeKey{origin, primary.ID().String()}]; route != nil {
				if _, semanticParams.NewName, err = s.prepareMapperRename(ctx, snapshot, service, params.TextDocument.Uri, params.Position, route, &params.NewName); err != nil {
					return response, err
				}
			}
			orchestrator := &frozenEditProjects{snapshot, primary, params.TextDocument.Uri}
			edits, err := service.GetRenameEdits(ctx, &semanticParams, orchestrator)
			if err != nil {
				return response, err
			}
			providers := make(map[string]editprojection.Provider)
			var sources []*ast.SourceFile
			for _, edit := range edits {
				sources = append(sources, edit.File)
				if route := routes[routeKey{edit.File, edit.Context}]; route != nil {
					providers[route.owner(edit.Context)] = route.provider
				}
			}
			if len(providers) == 0 {
				return service.ProvideRename(ctx, &semanticParams, orchestrator)
			}
			if !s.clientCapabilities.Workspace.WorkspaceEdit.DocumentChanges {
				return response, errors.New("projected rename requires versioned documentChanges support")
			}
			if !validProjectedRenameName(semanticParams.NewName) {
				return response, errors.New("edit projection: expected a canonical identifier")
			}
			plan, err := editprojection.NewPlan(editSnapshot(snapshot, programs), editprojection.Operation{Kind: editprojection.Rename, NewName: semanticParams.NewName}, edits, views...)
			if err != nil {
				return response, err
			}
			response.WorkspaceEdit, err = plan.Project(ctx, providers, s.editFreshness(ctx, params.TextDocument.Uri, snapshot, sources), s.positionEncoding)
			return response, err
		}
	})
	return work
}

func (s *Server) tryMapperImportActions(ctx context.Context, service *ls.LanguageService, params *lsproto.CodeActionParams) (response lsproto.CodeActionResponse, handled bool, err error) {
	if params.Context == nil || params.Context.Only == nil {
		return response, false, nil
	}
	file := service.GetProgram().GetSourceFile(params.TextDocument.Uri.FileName())
	route := editRoute(service.GetProgram(), file, editprojection.OrganizeImports)
	if route == nil {
		return
	}
	handled = true
	s.session.WithSnapshotForDocument(ctx, params.TextDocument.Uri, func(snapshot *project.Snapshot) {
		proj := snapshot.GetDefaultProject(params.TextDocument.Uri)
		if proj == nil || proj.GetProgram() != service.GetProgram() {
			err = lsproto.ErrorCodeContentModified
			return
		}
		response, err = service.ProvideCodeActionsWithImportProjection(ctx, params, func(kind lsproto.CodeActionKind) (*lsproto.WorkspaceEdit, error) {
			if !s.clientCapabilities.Workspace.WorkspaceEdit.DocumentChanges {
				return nil, errors.New("projected import actions require versioned documentChanges support")
			}
			plan, planErr := service.GetOrganizeImportsEditPlan(ctx, params.TextDocument.Uri, kind, editSnapshot(snapshot, []*compiler.Program{service.GetProgram()}))
			if planErr != nil {
				return nil, planErr
			}
			return plan.Project(ctx, map[string]editprojection.Provider{file.ContentMapper(): route.provider}, s.editFreshness(ctx, params.TextDocument.Uri, snapshot, []*ast.SourceFile{file}), s.positionEncoding)
		})
	})
	return
}

type originalEditScript struct{ file *ast.SourceFile }

func (s originalEditScript) FileName() tspath.RootedFilePath { return s.file.OriginalFileName() }
func (s originalEditScript) OriginalFileName() tspath.RootedFilePath {
	return s.file.OriginalFileName()
}
func (s originalEditScript) Text() string            { return s.file.OriginalText() }
func (s originalEditScript) OriginalText() string    { return s.file.OriginalText() }
func (originalEditScript) SpanMap() *spanmap.SpanMap { return nil }
