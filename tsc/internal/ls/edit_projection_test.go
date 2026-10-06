package ls

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/compiler"
	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/ls/editprojection"
	"github.com/microsoft/TypeScript/tsc/internal/ls/lsconv"
	"github.com/microsoft/TypeScript/tsc/internal/ls/lsutil"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/parser"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
	"github.com/microsoft/TypeScript/tsc/internal/testutil/contentmappertest"
	"github.com/microsoft/TypeScript/tsc/internal/tsoptions"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"github.com/microsoft/TypeScript/tsc/internal/vfs"
	"github.com/microsoft/TypeScript/tsc/internal/vfs/vfstest"
	"gotest.tools/v3/assert"
)

// Build single-program plans in tests using the same generated-edits entry point as LSP.
func (l *LanguageService) getRenameEditPlan(ctx context.Context, params *lsproto.RenameParams, snapshot editprojection.Snapshot) (*editprojection.Plan, error) {
	edits, err := l.GetRenameEdits(ctx, params, nil)
	if err != nil {
		return nil, err
	}
	var views []editprojection.SourceProjection
	for _, file := range l.program.GetSourceFiles() {
		view := editprojection.SourceProjection{File: file}
		if l.projectID != nil {
			view.Context = l.projectID.String()
		}
		views = append(views, view)
	}
	return editprojection.NewPlan(snapshot, editprojection.Operation{Kind: editprojection.Rename, NewName: params.NewName}, edits, views...)
}

func TestProjectedRenameMixedSpellings(t *testing.T) {
	t.Parallel()
	// app.view projects to an import followed by child.saveItem for each listener. The listener
	// names are whole-token Atoms; imports, access prefixes and punctuation are synthesized.
	// With duplicate=true, each authored name has two generated references to the SAME symbol.
	const authored = "<!-- 😀 -->\n<Child @saveItem=\"handler\" @save-item=\"handler\" />\n"
	for _, duplicate := range []bool{false, true} {
		for _, origin := range []string{"declaration", "camel", "kebab"} {
			t.Run(fmt.Sprintf("%s/duplicate=%t", origin, duplicate), func(t *testing.T) {
				t.Parallel()
				files := map[string]string{"/child.ts": "export const child = { saveItem: 1 };\n", "/app.view": authored}
				transform := contentmappertest.ListenerTransform(duplicate, false)
				service := newProjectionTestService(t, files, transform)
				name, offset := "/child.ts", strings.Index(files["/child.ts"], "saveItem")
				if origin != "declaration" {
					name = "/app.view"
					spelling := "saveItem"
					if origin == "kebab" {
						spelling = "save-item"
					}
					offset = strings.Index(authored, spelling)
					for i := range len(spelling) {
						info, err := service.PrepareProjectedRename(t.Context(), uri(name), originalPosition(service, name, files[name], offset+i))
						assert.NilError(t, err)
						assert.Assert(t, info.CanRename)
						assert.Equal(t, info.TriggerSpan.Start, originalPosition(service, name, files[name], offset))
						assert.Equal(t, info.TriggerSpan.End, originalPosition(service, name, files[name], offset+len(spelling)))
					}
					// The legacy entry point remains exact-only.
					assert.Assert(t, !service.GetRenameInfo(t.Context(), "nextItem", uri(name), originalPosition(service, name, files[name], offset)).CanRename)
				}
				params := &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: uri(name)}, Position: originalPosition(service, name, files[name], offset), NewName: "nextItem"}
				plan, err := service.getRenameEditPlan(t.Context(), params, editprojection.Snapshot{ID: "1"})
				assert.NilError(t, err)
				calls := 0
				provider := func(ctx context.Context, request editprojection.Request) (editprojection.Response, error) {
					calls++
					// Re-entering the checker here must not deadlock: the plan owns value data only.
					assert.Equal(t, len(service.program.GetSemanticDiagnostics(ctx, service.program.GetSourceFile("/app.view"))), 0)
					return projectListeners(ctx, request)
				}
				owner := service.program.GetSourceFile("/app.view").ContentMapper()
				workspace, err := plan.Project(t.Context(), map[string]editprojection.Provider{owner: provider}, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
				assert.NilError(t, err)
				assert.Equal(t, calls, 1)
				updated := applyProjectionWorkspace(t, files, workspace)
				assert.Equal(t, updated["/child.ts"], "export const child = { nextItem: 1 };\n")
				assert.Equal(t, updated["/app.view"], strings.ReplaceAll(strings.ReplaceAll(authored, "saveItem", "nextItem"), "save-item", "next-item"))
				regenerated := newProjectionTestService(t, updated, transform)
				assertNoProjectionErrors(t, regenerated)
				virtual := regenerated.program.GetSourceFile("/app.view")
				assert.Assert(t, !strings.Contains(virtual.Text(), "saveItem"))
				assert.Equal(t, virtual.SpanMap().Segments()[0].Kind, spanmap.KindAtom)
			})
		}
	}
}

func TestProjectedRenameRejectsAmbiguity(t *testing.T) {
	t.Parallel()
	files := map[string]string{"/child.ts": "export const child = { saveItem: 1 }; export const other = { saveItem: 2 };", "/app.view": "<Child @save-item=\"handler\" />"}
	service := newProjectionTestService(t, files, contentmappertest.ListenerTransform(true, true))
	pos := originalPosition(service, "/app.view", files["/app.view"], strings.Index(files["/app.view"], "save-item")+3)
	_, err := service.PrepareProjectedRename(t.Context(), uri("/app.view"), pos)
	assert.ErrorContains(t, err, "ambiguous")
	plan, err := service.getRenameEditPlan(t.Context(), &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: uri("/app.view")}, Position: pos, NewName: "nextItem"}, editprojection.Snapshot{ID: "1"})
	assert.Assert(t, plan == nil)
	assert.ErrorContains(t, err, "ambiguous")
	declaration := originalPosition(service, "/child.ts", files["/child.ts"], strings.Index(files["/child.ts"], "saveItem"))
	plan, err = service.getRenameEditPlan(t.Context(), &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: uri("/child.ts")}, Position: declaration, NewName: "nextItem"}, editprojection.Snapshot{ID: "1"})
	assert.Assert(t, plan == nil)
	assert.ErrorContains(t, err, "uncovered rename projection")
}

func TestProjectedRenameMissingProviderReturnsNoPartialEdit(t *testing.T) {
	t.Parallel()
	files := map[string]string{"/child.ts": "export const child = { saveItem: 1 };", "/app.view": "<Child @save-item=\"handler\" />"}
	service := newProjectionTestService(t, files, contentmappertest.ListenerTransform(false, false))
	pos := originalPosition(service, "/child.ts", files["/child.ts"], strings.Index(files["/child.ts"], "saveItem"))
	plan, err := service.getRenameEditPlan(t.Context(), &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: uri("/child.ts")}, Position: pos, NewName: "nextItem"}, editprojection.Snapshot{ID: "1"})
	assert.NilError(t, err)
	workspace, err := plan.Project(t.Context(), nil, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
	assert.Assert(t, workspace == nil)
	assert.ErrorContains(t, err, "no provider")
}

func TestProjectedRenamePreservesNativeAliasEdits(t *testing.T) {
	t.Parallel()
	files := map[string]string{"/plain.ts": "const old = 1;\nconst object = { old };\nexport { old };\n"}
	service := newProjectionTestService(t, files, nil)
	params := &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: uri("/plain.ts")}, Position: originalPosition(service, "/plain.ts", files["/plain.ts"], 6), NewName: "next"}
	plan, err := service.getRenameEditPlan(t.Context(), params, editprojection.Snapshot{ID: "1"})
	assert.NilError(t, err)
	projected, err := plan.Project(t.Context(), nil, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
	assert.NilError(t, err)
	native, err := service.ProvideRename(t.Context(), params, nil)
	assert.NilError(t, err)
	assert.Assert(t, native.WorkspaceEdit != nil)
	actual := applyProjectionWorkspace(t, files, projected)
	assert.DeepEqual(t, actual, applyProjectionWorkspace(t, files, native.WorkspaceEdit))
	assert.Assert(t, strings.Contains(actual["/plain.ts"], "old: next"))
	assert.Assert(t, strings.Contains(actual["/plain.ts"], "next as old"))
}

func TestProjectedRenameDoesNotIgnoreHiddenPartialProjections(t *testing.T) {
	t.Parallel()
	files := map[string]string{"/child.ts": "export const child = { saveItem: 1 }; export const other = { saveItem: 2 };", "/app.view": "<Child @save-item=\"handler\" />"}
	transform := func(text string) contentmapper.Result {
		result := contentmappertest.ListenerTransform(true, true)(text)
		segments := slices.Clone(result.Mappings.Segments())
		segments[1].Features = spanmap.FeatureNone
		segments[1].OriginalEnd = segments[1].OriginalStart + 4
		result.Mappings = spanmap.New(segments)
		return result
	}
	service := newProjectionTestService(t, files, transform)
	params := &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: uri("/child.ts")}, Position: originalPosition(service, "/child.ts", files["/child.ts"], strings.Index(files["/child.ts"], "saveItem")), NewName: "nextItem"}
	plan, err := service.getRenameEditPlan(t.Context(), params, editprojection.Snapshot{ID: "1"})
	assert.Assert(t, plan == nil)
	assert.ErrorContains(t, err, "uncovered rename projection")
}

func TestProjectedOrganizeImports(t *testing.T) {
	t.Parallel()
	// The mapper collapses the authored import to one generated line (an Atom) and prefixes an
	// unused synthesized helper import. The body is verbatim after removing the @body delimiter.
	// TypeScript deletes the helper and sorts/removes names; the provider reconstructs only the
	// authored import and retains its comment and multiline layout.
	const original = "// keep this comment\nimport {\n    z,\n    a,\n    unused,\n} from \"./dep\";\n\n@body { a; z; }\n"
	files := map[string]string{"/app.view": original, "/dep.ts": "export const a = 1, z = 2, unused = 3;", "/runtime.ts": "export const helper = 1;"}
	service := newProjectionTestService(t, files, contentmappertest.ImportTransform)
	file := service.program.GetSourceFile("/app.view")
	legacy := service.OrganizeImports(t.Context(), file, service.program, lsproto.CodeActionKindSourceOrganizeImportsTs)
	assert.Equal(t, len(legacy), 0)
	plan, err := service.GetOrganizeImportsEditPlan(t.Context(), uri("/app.view"), lsproto.CodeActionKindSourceOrganizeImportsTs, editprojection.Snapshot{ID: "1"})
	assert.NilError(t, err)
	calls := 0
	provider := func(ctx context.Context, req editprojection.Request) (editprojection.Response, error) {
		calls++
		assert.Equal(t, req.Operation.Kind, editprojection.OrganizeImports)
		assert.Equal(t, len(req.Projections), 1)
		projection := req.Projections[0]
		var edits []core.TextChange
		var ids []int
		for _, edit := range req.Edits {
			edits = append(edits, edit.Change)
			ids = append(ids, edit.ID)
		}
		assert.Assert(t, len(edits) >= 2)
		generated := core.ApplyBulkEdits(projection.Text, edits)
		assert.Assert(t, !strings.Contains(generated, "helper"))
		parsed := parseProjectionFile("/generated.ts", generated)
		var names []string
		for _, statement := range parsed.Statements.Nodes {
			if ast.IsImportDeclaration(statement) && statement.AsImportDeclaration().ModuleSpecifier.Text() == "./dep" {
				bindings := statement.AsImportDeclaration().ImportClause.AsImportClause().NamedBindings.AsNamedImports()
				for _, specifier := range bindings.Elements.Nodes {
					names = append(names, specifier.Name().Text())
				}
			}
		}
		assert.DeepEqual(t, names, []string{"a", "z"})
		document := req.Documents[0]
		start := strings.Index(document.Text, "import {")
		end := start + strings.Index(document.Text[start:], ";") + 1
		text := "import {\n    " + strings.Join(names, ",\n    ") + ",\n} from \"./dep\";"
		return editprojection.Response{Snapshot: req.Snapshot, Results: []editprojection.Result{{Inputs: ids, Edits: []editprojection.AuthoredEdit{{Document: document.ID, Change: core.TextChange{TextRange: core.NewTextRange(start, end), NewText: text}}}}}}, nil
	}
	workspace, err := plan.Project(t.Context(), map[string]editprojection.Provider{file.ContentMapper(): provider}, func() string { return "1" }, lsproto.PositionEncodingKindUTF8)
	assert.NilError(t, err)
	assert.Equal(t, calls, 1)
	updated := applyProjectionWorkspace(t, files, workspace)
	assert.Equal(t, updated["/app.view"], "// keep this comment\nimport {\n    a,\n    z,\n} from \"./dep\";\n\n@body { a; z; }\n")
	regenerated := newProjectionTestService(t, updated, contentmappertest.ImportTransform)
	assertNoProjectionErrors(t, regenerated)
	// Regeneration legitimately restores the synthesized helper. Equality to patched virtual text
	// would therefore be the wrong correctness criterion.
	assert.Assert(t, strings.Contains(regenerated.program.GetSourceFile("/app.view").Text(), "helper"))
}

func projectListeners(ctx context.Context, req editprojection.Request) (editprojection.Response, error) {
	response := editprojection.Response{Snapshot: req.Snapshot}
	for _, edit := range req.Edits {
		var projection editprojection.Projection
		for _, candidate := range req.Projections {
			if candidate.ID == edit.Projection {
				projection = candidate
				break
			}
		}
		var document editprojection.Document
		for _, candidate := range req.Documents {
			if candidate.ID == projection.Document {
				document = candidate
				break
			}
		}
		mapping := spanmap.New(projection.Mappings)
		rng, fidelity := mapping.VirtualToOriginalSpan(edit.Change.TextRange)
		if !fidelity.IsSingleSegment() || edit.Change.NewText != req.Operation.NewName {
			return editprojection.Response{}, errors.New("unsupported listener edit")
		}
		name := contentmappertest.RenameListener(document.Text[rng.Pos():rng.End()], edit.Change.NewText)
		response.Results = append(response.Results, editprojection.Result{Inputs: []int{edit.ID}, Edits: []editprojection.AuthoredEdit{{Document: document.ID, Change: core.TextChange{TextRange: rng, NewText: name}}}})
	}
	return response, ctx.Err()
}

type projectionTestMapper struct {
	transform func(string) contentmapper.Result
}

func (projectionTestMapper) Refresh() error                                 { return nil }
func (projectionTestMapper) Identities() ([]string, error)                  { return []string{"1"}, nil }
func (projectionTestMapper) Identity(*contentmapper.Mapper) (string, error) { return "1", nil }
func (projectionTestMapper) WatchedFiles() ([]tspath.RootedFilePath, error) { return nil, nil }
func (projectionTestMapper) Diagnostics() []contentmapper.OptionDiagnostic  { return nil }
func (projectionTestMapper) Close() error                                   { return nil }
func (m projectionTestMapper) Transform(_ *contentmapper.Mapper, req contentmapper.Request) (contentmapper.Result, error) {
	return m.transform(req.Content), nil
}

type projectionTestHost struct {
	Host
	fs         vfs.FS
	converters *lsconv.Converters
}

func (h projectionTestHost) CaseSensitivity() tspath.CaseSensitivity { return h.fs.CaseSensitivity() }

func (h projectionTestHost) ReadFile(path tspath.RootedFilePath) (string, bool) {
	return h.fs.ReadFile(path)
}

func (h projectionTestHost) FileExists(path tspath.RootedFilePath) bool { return h.fs.FileExists(path) }
func (h projectionTestHost) Converters() *lsconv.Converters             { return h.converters }
func (projectionTestHost) GetPreferences(string) lsutil.UserPreferences {
	return lsutil.UserPreferences{FormatCodeSettings: lsutil.GetDefaultFormatCodeSettings()}
}

func newProjectionTestService(t *testing.T, files map[string]string, transform func(string) contentmapper.Result) *LanguageService {
	t.Helper()
	fs := bundled.WrapFS(vfstest.FromMap(files, tspath.CaseSensitive))
	var roots []tspath.RootedFilePath
	for name := range files {
		roots = append(roots, tspath.RootedFilePathFromNormalized(name))
	}
	slices.Sort(roots)
	options := &core.CompilerOptions{Module: core.ModuleKindESNext, ModuleResolution: core.ModuleResolutionKindBundler, SkipLibCheck: core.TSTrue}
	config := tsoptions.NewParsedCommandLine(options, roots, nil, "/", fs.CaseSensitivity())
	config.ParsedConfig.ContentMappers = []*contentmapper.Mapper{{Package: "prototype", Extensions: []string{".view"}, Name: "prototype", Version: "1"}}
	program := compiler.NewProgram(compiler.ProgramOptions{Config: config, Host: compiler.NewCompilerHost(fs, bundled.LibPath(), nil, nil, projectionTestMapper{transform}), SingleThreaded: core.TSTrue})
	converters := lsconv.NewConverters(lsproto.PositionEncodingKindUTF8, func(name tspath.RootedFilePath) *lsconv.LSPLineMap {
		text, _ := fs.ReadFile(name)
		return lsconv.ComputeLSPLineStarts(text)
	})
	return NewLanguageService(nil, program, projectionTestHost{fs: fs, converters: converters}, "/app.view")
}

func uri(name string) lsproto.DocumentUri {
	return lsconv.FileNameToDocumentURI(tspath.RootedFilePathFromNormalized(name))
}

func parseProjectionFile(name, text string) *ast.SourceFile {
	return parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: tspath.RootedFilePathFromNormalized(name), PathKey: tspath.PathKeyFromCanonical(name)}, text, core.ScriptKindTS)
}

func originalPosition(service *LanguageService, name, text string, offset int) lsproto.Position {
	position, _ := service.converters.ToLSPPosition(parseProjectionFile(name, text), core.TextPos(offset))
	return position
}

func applyProjectionWorkspace(t *testing.T, files map[string]string, workspace *lsproto.WorkspaceEdit) map[string]string {
	t.Helper()
	updated := maps.Clone(files)
	var changes []lsproto.TextDocumentEditOrCreateFileOrRenameFileOrDeleteFile
	if workspace.DocumentChanges != nil {
		changes = *workspace.DocumentChanges
	} else if workspace.Changes != nil {
		for uri, edits := range *workspace.Changes {
			document := &lsproto.TextDocumentEdit{TextDocument: lsproto.OptionalVersionedTextDocumentIdentifier{Uri: uri}}
			for _, edit := range edits {
				document.Edits = append(document.Edits, lsproto.TextEditOrAnnotatedTextEditOrSnippetTextEdit{TextEdit: edit})
			}
			changes = append(changes, lsproto.TextDocumentEditOrCreateFileOrRenameFileOrDeleteFile{TextDocumentEdit: document})
		}
	}
	for _, change := range changes {
		doc := change.TextDocumentEdit
		assert.Assert(t, doc != nil)
		name := doc.TextDocument.Uri.FileName().AsString()
		text, exists := files[name]
		assert.Assert(t, exists, "generated document escaped: %s", name)
		converters := lsconv.NewConverters(lsproto.PositionEncodingKindUTF8, func(tspath.RootedFilePath) *lsconv.LSPLineMap { return lsconv.ComputeLSPLineStarts(text) })
		file := parseProjectionFile(name, text)
		var edits []core.TextChange
		for _, edit := range doc.Edits {
			spans := converters.FromLSPRange(file, edit.TextEdit.Range, spanmap.FeatureAll)
			edits = append(edits, core.TextChange{TextRange: spans[0].Span, NewText: edit.TextEdit.NewText})
		}
		slices.SortFunc(edits, func(a, b core.TextChange) int { return a.Pos() - b.Pos() })
		updated[name] = core.ApplyBulkEdits(text, edits)
	}
	return updated
}

func assertNoProjectionErrors(t *testing.T, service *LanguageService) {
	t.Helper()
	for _, file := range service.program.GetSourceFiles() {
		if file.IsDeclarationFile {
			continue
		}
		assert.Equal(t, len(service.program.GetSyntacticDiagnostics(t.Context(), file)), 0, file.FileName())
		assert.Equal(t, len(service.program.GetSemanticDiagnostics(t.Context(), file)), 0, file.FileName())
	}
}
