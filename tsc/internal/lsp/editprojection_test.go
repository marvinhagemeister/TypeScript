package lsp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/ipc"
	"github.com/microsoft/TypeScript/tsc/internal/json"
	"github.com/microsoft/TypeScript/tsc/internal/ls/lsconv"
	"github.com/microsoft/TypeScript/tsc/internal/lsp"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/parser"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
	"github.com/microsoft/TypeScript/tsc/internal/testutil/contentmappertest"
	"github.com/microsoft/TypeScript/tsc/internal/testutil/lsptestutil"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"github.com/microsoft/TypeScript/tsc/internal/vfs"
	"github.com/microsoft/TypeScript/tsc/internal/vfs/vfstest"
	"gotest.tools/v3/assert"
)

const projectionComponent = "<!-- 😀 -->\n<Child @saveItem=\"handler\" @save-item=\"handler\" />\n"

// The successful integration tests launch this test executable as a separate mapper process. There
// are two independent, Content-Length-framed JSON-RPC boundaries: client/server and server/mapper.
func TestEditProjectionMapperProcess(t *testing.T) {
	t.Parallel()
	if os.Getenv("TS_EDIT_MAPPER_PROCESS") != "1" {
		return
	}
	conn := ipc.NewAsyncConn(mapperStdio{os.Stdin, os.Stdout}, &rpcEditMapper{})
	_ = conn.Run(context.Background())
	os.Exit(0)
}

type mapperStdio struct {
	io.Reader
	io.Writer
}

func (mapperStdio) Close() error { return nil }

type mapperChild struct {
	io.ReadCloser
	io.WriteCloser
	cmd  *exec.Cmd
	once sync.Once
}

func (p *mapperChild) Close() error {
	p.once.Do(func() {
		_ = p.WriteCloser.Close()
		_ = p.ReadCloser.Close()
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	})
	return nil
}

func spawnEditMapper(_ []string, _ string, stderr io.Writer) (io.ReadWriteCloser, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable, "-test.run=^TestEditProjectionMapperProcess$")
	cmd.Env = append(os.Environ(), "TS_EDIT_MAPPER_PROCESS=1")
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		_ = out.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return nil, err
	}
	return &mapperChild{ReadCloser: out, WriteCloser: in, cmd: cmd}, nil
}

type rpcEditMapper struct {
	mode          string
	onProject     func(contentmapper.ProjectEditsParams)
	onPrepare     func(contentmapper.PrepareRenameParams)
	prepareResult func(contentmapper.PrepareRenameParams) (contentmapper.PrepareRenameResult, error)
}

func (*rpcEditMapper) HandleNotification(context.Context, string, json.Value) error { return nil }
func (h *rpcEditMapper) HandleRequest(ctx context.Context, method string, raw json.Value) (any, error) {
	switch method {
	case contentmapper.MethodInitialize:
		return contentmapper.InitializeResult{PositionEncoding: contentmapper.PositionEncodingUTF8, DiagnosticSource: "edit-test"}, nil
	case contentmapper.MethodOpenProject:
		var params contentmapper.OpenProjectParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if params.EditProjectionVersion != contentmapper.EditProjectionVersion {
			return nil, errors.New("host did not advertise editing")
		}
		result := contentmapper.OpenProjectResult{}
		if h.mode != "legacy" {
			result.EditProjection = &contentmapper.EditProjectionCapabilities{Version: contentmapper.EditProjectionVersion, Rename: true, OrganizeImports: true, RenameInput: h.mode != "canonical-input"}
		}
		return result, nil
	case contentmapper.MethodCloseProject:
		return nil, nil
	case contentmapper.MethodPrepareRename:
		var params contentmapper.PrepareRenameParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if params.PositionEncoding != contentmapper.PositionEncodingUTF8 || params.Start < 0 || params.End > len(params.Content) {
			return nil, errors.New("invalid preparation range")
		}
		if h.onPrepare != nil {
			h.onPrepare(params)
		}
		if h.mode == "prepare-hang" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if h.prepareResult != nil {
			return h.prepareResult(params)
		}
		result := contentmapper.PrepareRenameResult{Snapshot: params.Snapshot, CanRename: h.mode != "reject-prepare", Message: "fixture rejection"}
		if h.mode != "canonical-input" {
			result.Placeholder = new(params.Content[params.Start:params.End])
			if params.NewName != nil {
				// This fixture accepts an optional Vue-style sigil; production has no such policy.
				name := strings.TrimPrefix(*params.NewName, "@")
				var canonical strings.Builder
				for i, part := range strings.Split(name, "-") {
					if part == "" {
						result.CanRename = false
						break
					}
					if i > 0 {
						runes := []rune(part)
						runes[0] = unicode.ToUpper(runes[0])
						part = string(runes)
					}
					canonical.WriteString(part)
				}
				result.NormalizedName = new(canonical.String())
			}
		}
		return result, nil
	case contentmapper.MethodTransform:
		var params contentmapper.TransformParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		transform := contentmappertest.ListenerTransform(true, false)
		if strings.Contains(params.Content, "@body ") {
			transform = contentmappertest.ImportTransform
		}
		result := transform(params.Content)
		mapping, err := result.Mappings.Marshal()
		if err != nil {
			return nil, err
		}
		return contentmapper.TransformResult{Text: result.Text, Extension: result.VirtualExtension, Mappings: json.Value(mapping)}, nil
	case contentmapper.MethodProjectEdits:
		var params contentmapper.ProjectEditsParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if params.PositionEncoding != contentmapper.PositionEncodingUTF8 {
			return nil, errors.New("editing offsets must be UTF-8")
		}
		if h.onProject != nil {
			h.onProject(params)
		}
		if h.mode == "failure" {
			return nil, errors.New("fixture provider failed")
		}
		if h.mode == "hang" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		result, err := projectEditFixture(params)
		switch h.mode {
		case "stale":
			result.Snapshot = "old"
		case "missing":
			result.Results = nil
		case "unauthorized":
			result.Results[0].Edits[0].Document = 9999
		case "range":
			result.Results[0].Edits[0].End = 999999
		case "overflow":
			result.Results[0].Edits[0].Start += 1 << 32
			result.Results[0].Edits[0].End += 1 << 32
		case "conflict":
			result.Results = append(result.Results, contentmapper.EditCoverage{Inputs: []int{params.Edits[0].ID}, Edits: []contentmapper.AuthoredEdit{{Document: params.Documents[0].ID, Start: 0, End: 1, NewText: "bad"}}})
		}
		return result, err
	}
	return nil, fmt.Errorf("unexpected mapper method %s", method)
}

func projectEditFixture(params contentmapper.ProjectEditsParams) (contentmapper.ProjectEditsResult, error) {
	result := contentmapper.ProjectEditsResult{Snapshot: params.Snapshot}
	projections := make(map[int]contentmapper.EditProjection)
	documents := make(map[int]contentmapper.EditDocument)
	for _, projection := range params.Projections {
		projections[projection.ID] = projection
	}
	for _, document := range params.Documents {
		documents[document.ID] = document
	}
	if params.Operation == "organizeImports" {
		projection := params.Projections[0]
		var edits []core.TextChange
		var inputs []int
		for _, edit := range params.Edits {
			inputs = append(inputs, edit.ID)
			edits = append(edits, core.TextChange{TextRange: core.NewTextRange(edit.Start, edit.End), NewText: edit.NewText})
		}
		slices.SortFunc(edits, func(a, b core.TextChange) int { return a.Pos() - b.Pos() })
		generated := core.ApplyBulkEdits(projection.Text, edits)
		file := editTestSource("/generated.ts", generated)
		var names []string
		for _, statement := range file.Statements.Nodes {
			if ast.IsImportDeclaration(statement) && statement.AsImportDeclaration().ModuleSpecifier.Text() == "./dep" {
				for _, specifier := range statement.AsImportDeclaration().ImportClause.AsImportClause().NamedBindings.AsNamedImports().Elements.Nodes {
					names = append(names, specifier.Name().Text())
				}
			}
		}
		doc := documents[projection.Document]
		start := strings.Index(doc.Text, "import {")
		end := start + strings.Index(doc.Text[start:], ";") + 1
		text := "import {\n    " + strings.Join(names, ",\n    ") + ",\n} from \"./dep\";"
		result.Results = []contentmapper.EditCoverage{{Inputs: inputs, Edits: []contentmapper.AuthoredEdit{{Document: doc.ID, Start: start, End: end, NewText: text}}}}
		return result, nil
	}
	for _, edit := range params.Edits {
		projection := projections[edit.Projection]
		doc := documents[projection.Document]
		mapping, err := spanmap.Unmarshal(projection.Mappings)
		if err != nil {
			return result, err
		}
		rng, fidelity := mapping.VirtualToOriginalSpan(core.NewTextRange(edit.Start, edit.End))
		if !fidelity.IsSingleSegment() || edit.NewText != params.NewName {
			return result, errors.New("unsupported listener edit")
		}
		name := contentmappertest.RenameListener(doc.Text[rng.Pos():rng.End()], params.NewName)
		result.Results = append(result.Results, contentmapper.EditCoverage{Inputs: []int{edit.ID}, Edits: []contentmapper.AuthoredEdit{{Document: doc.ID, Start: rng.Pos(), End: rng.End(), NewText: name}}})
	}
	return result, nil
}

func editTestFiles() map[string]string {
	return map[string]string{
		"/home/project/tsconfig.json":                    `{"compilerOptions":{"target":"es2020","module":"esnext","moduleResolution":"bundler","strict":true},"contentMappers":[{"package":"mapper","extensions":[".view"]}]}`,
		"/home/project/node_modules/mapper/package.json": contentmappertest.PackageJSON("projection-mapper"),
		"/home/project/child.ts":                         "export const child = { saveItem: 1 };\n",
		"/home/project/app.view":                         projectionComponent,
	}
}

func newEditLSPClient(t *testing.T, files map[string]string, mapper *rpcEditMapper, encoding lsproto.PositionEncodingKind) *lsptestutil.LSPClient {
	t.Helper()
	client, _ := newEditLSPClientWithFS(t, files, mapper, encoding)
	return client
}

func newEditLSPClientWithFS(t *testing.T, files map[string]string, mapper *rpcEditMapper, encoding lsproto.PositionEncodingKind, documentChanges ...bool) (*lsptestutil.LSPClient, vfs.FS) {
	t.Helper()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}
	spawn := spawnEditMapper
	if mapper != nil {
		spawn = func(_ []string, _ string, _ io.Writer) (io.ReadWriteCloser, error) {
			client, server := net.Pipe()
			conn := ipc.NewAsyncConn(server, mapper)
			go func() { _ = conn.Run(t.Context()); _ = server.Close() }()
			return client, nil
		}
	}
	disk := vfstest.FromMap(files, tspath.CaseSensitive)
	fs := bundled.WrapFS(disk)
	client, closeClient := lsptestutil.NewLSPClient(t, lsp.ServerOptions{Err: io.Discard, Cwd: "/home/project", FS: fs, DefaultLibraryPath: bundled.LibPath(), Spawn: spawn}, func(_ context.Context, request *lsproto.RequestMessage) *lsproto.ResponseMessage {
		if request.Method == lsproto.MethodWorkspaceConfiguration {
			return &lsproto.ResponseMessage{ID: request.ID, JSONRPC: request.JSONRPC, Result: []any{nil, nil, nil, nil}}
		}
		return &lsproto.ResponseMessage{ID: request.ID, JSONRPC: request.JSONRPC, Result: lsproto.Null{}}
	})
	t.Cleanup(func() { _ = closeClient() })
	supportsDocumentChanges := len(documentChanges) == 0 || documentChanges[0]
	msg, _, ok := client.SendRequest(t, lsproto.InitializeInfo, &lsproto.InitializeParams{
		Capabilities:          &lsproto.ClientCapabilities{Workspace: &lsproto.WorkspaceClientCapabilities{WorkspaceEdit: &lsproto.WorkspaceEditClientCapabilities{DocumentChanges: new(supportsDocumentChanges)}}, General: &lsproto.GeneralClientCapabilities{PositionEncodings: new([]lsproto.PositionEncodingKind{encoding})}},
		InitializationOptions: &lsproto.InitializationOptionsOrNull{InitializationOptions: &lsproto.InitializationOptions{RunExternalCode: new(true)}},
	})
	assert.Assert(t, ok && msg.AsResponse().Error == nil)
	client.SendNotification(t, lsproto.InitializedInfo, &lsproto.InitializedParams{})
	<-client.Server.InitComplete()
	return client, disk
}

func changeEditDocument(t *testing.T, client *lsptestutil.LSPClient, name, text string, version int32) {
	t.Helper()
	client.SendNotification(t, lsproto.TextDocumentDidChangeInfo, &lsproto.DidChangeTextDocumentParams{TextDocument: lsproto.VersionedTextDocumentIdentifier{Uri: editTestURI(name), Version: version}, ContentChanges: []lsproto.TextDocumentContentChangePartialOrWholeDocument{{WholeDocument: &lsproto.TextDocumentContentChangeWholeDocument{Text: text}}}})
}

func assertEditDiagnostics(t *testing.T, client *lsptestutil.LSPClient, name string) {
	t.Helper()
	msg, diagnostics, ok := client.SendRequest(t, lsproto.TextDocumentDiagnosticInfo, &lsproto.DocumentDiagnosticParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI(name)}})
	assert.Assert(t, ok && msg.AsResponse().Error == nil)
	assert.Assert(t, diagnostics.FullDocumentDiagnosticReport != nil)
	assert.Equal(t, len(diagnostics.FullDocumentDiagnosticReport.Items), 0, name)
}

func openEditDocument(t *testing.T, client *lsptestutil.LSPClient, name, text string, version int32) {
	t.Helper()
	language := "typescript"
	if strings.HasSuffix(name, ".view") {
		language = "view"
	}
	client.SendNotification(t, lsproto.TextDocumentDidOpenInfo, &lsproto.DidOpenTextDocumentParams{TextDocument: &lsproto.TextDocumentItem{Uri: editTestURI(name), LanguageId: lsproto.LanguageKind(language), Version: version, Text: text}})
}

func editTestURI(name string) lsproto.DocumentUri {
	return lsconv.FileNameToDocumentURI(tspath.RootedFilePathFromNormalized(name))
}

func editTestSource(name, text string) *ast.SourceFile {
	return parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: tspath.RootedFilePathFromNormalized(name), PathKey: tspath.PathKeyFromCanonical(name)}, text, core.ScriptKindTS)
}

func editTestPosition(name, text string, offset int, encoding lsproto.PositionEncodingKind) lsproto.Position {
	converters := lsconv.NewConverters(encoding, func(tspath.RootedFilePath) *lsconv.LSPLineMap { return lsconv.ComputeLSPLineStarts(text) })
	position, _ := converters.ToLSPPosition(editTestSource(name, text), core.TextPos(offset))
	return position
}

func applyLSPEdit(t *testing.T, files map[string]string, workspace *lsproto.WorkspaceEdit, encoding lsproto.PositionEncodingKind) map[string]string {
	t.Helper()
	assert.Assert(t, workspace != nil && workspace.DocumentChanges != nil)
	updated := maps.Clone(files)
	for _, change := range *workspace.DocumentChanges {
		doc := change.TextDocumentEdit
		assert.Assert(t, doc != nil)
		name := doc.TextDocument.Uri.FileName().AsString()
		text, exists := files[name]
		assert.Assert(t, exists, "generated destination escaped: %s", name)
		converters := lsconv.NewConverters(encoding, func(tspath.RootedFilePath) *lsconv.LSPLineMap { return lsconv.ComputeLSPLineStarts(text) })
		var edits []core.TextChange
		for _, edit := range doc.Edits {
			spans := converters.FromLSPRange(editTestSource(name, text), edit.TextEdit.Range, spanmap.FeatureAll)
			edits = append(edits, core.TextChange{TextRange: spans[0].Span, NewText: edit.TextEdit.NewText})
		}
		slices.SortFunc(edits, func(a, b core.TextChange) int { return a.Pos() - b.Pos() })
		updated[name] = core.ApplyBulkEdits(text, edits)
	}
	return updated
}

func TestLSPProjectedRenameProcess(t *testing.T) {
	t.Parallel()
	for _, encoding := range []lsproto.PositionEncodingKind{lsproto.PositionEncodingKindUTF8, lsproto.PositionEncodingKindUTF16} {
		for _, origin := range []string{"declaration", "camel", "kebab"} {
			t.Run(string(encoding)+"/"+origin, func(t *testing.T) {
				t.Parallel()
				files := editTestFiles()
				client := newEditLSPClient(t, files, nil, encoding)
				name, spelling := "/home/project/child.ts", "saveItem"
				if origin != "declaration" {
					name = "/home/project/app.view"
					if origin == "kebab" {
						spelling = "save-item"
					}
				}
				openEditDocument(t, client, name, files[name], 7)
				position := editTestPosition(name, files[name], strings.Index(files[name], spelling)+2, encoding)
				msg, prepared, ok := client.SendRequest(t, lsproto.TextDocumentPrepareRenameInfo, &lsproto.PrepareRenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI(name)}, Position: position})
				assert.Assert(t, ok && msg.AsResponse().Error == nil, "prepare: %v", msg.AsResponse().Error)
				assert.Assert(t, prepared.PrepareRenamePlaceholder != nil)
				assert.Equal(t, prepared.PrepareRenamePlaceholder.Placeholder, spelling)
				msg, renamed, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI(name)}, Position: position, NewName: "nextItem"})
				assert.Assert(t, ok && msg.AsResponse().Error == nil, "rename: %v", msg.AsResponse().Error)
				updated := applyLSPEdit(t, files, renamed.WorkspaceEdit, encoding)
				assert.Equal(t, updated["/home/project/child.ts"], "export const child = { nextItem: 1 };\n")
				assert.Equal(t, updated["/home/project/app.view"], strings.ReplaceAll(strings.ReplaceAll(projectionComponent, "saveItem", "nextItem"), "save-item", "next-item"))
				for _, change := range *renamed.WorkspaceEdit.DocumentChanges {
					doc := change.TextDocumentEdit
					if doc.TextDocument.Uri == editTestURI(name) {
						assert.Assert(t, doc.TextDocument.Version.Integer != nil)
						assert.Equal(t, *doc.TextDocument.Version.Integer, int32(7))
					} else {
						assert.Assert(t, doc.TextDocument.Version.Integer == nil)
					}
				}
				// Apply authored edits and re-transform through the mapper process, not a direct fixture call.
				for _, changed := range []string{"/home/project/child.ts", "/home/project/app.view"} {
					if changed == name {
						changeEditDocument(t, client, changed, updated[changed], 8)
					} else {
						openEditDocument(t, client, changed, updated[changed], 8)
					}
				}
				assertEditDiagnostics(t, client, "/home/project/child.ts")
				assertEditDiagnostics(t, client, "/home/project/app.view")
			})
		}
	}
}

func TestLSPProjectedOrganizeImportsProcess(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	files["/home/project/app.view"] = "// keep this comment\nimport {\n    z,\n    a,\n    unused,\n} from \"./dep\";\n\n@body { a; z; }\n"
	files["/home/project/dep.ts"] = "export const a = 1, z = 2, unused = 3;"
	files["/home/project/runtime.ts"] = "export const helper = 1;"
	client := newEditLSPClient(t, files, nil, lsproto.PositionEncodingKindUTF16)
	openEditDocument(t, client, "/home/project/app.view", files["/home/project/app.view"], 3)
	msg, actions, ok := client.SendRequest(t, lsproto.TextDocumentCodeActionInfo, &lsproto.CodeActionParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI("/home/project/app.view")}, Context: &lsproto.CodeActionContext{Diagnostics: []*lsproto.Diagnostic{}, Only: new([]lsproto.CodeActionKind{lsproto.CodeActionKindSourceOrganizeImports})}})
	assert.Assert(t, ok && msg.AsResponse().Error == nil, "imports: %v", msg.AsResponse().Error)
	assert.Assert(t, actions.CommandOrCodeActionArray != nil)
	assert.Equal(t, len(*actions.CommandOrCodeActionArray), 1)
	action := (*actions.CommandOrCodeActionArray)[0].CodeAction
	assert.Assert(t, action != nil && action.Edit != nil)
	updated := applyLSPEdit(t, files, action.Edit, lsproto.PositionEncodingKindUTF16)
	assert.Equal(t, updated["/home/project/app.view"], "// keep this comment\nimport {\n    a,\n    z,\n} from \"./dep\";\n\n@body { a; z; }\n")
	changeEditDocument(t, client, "/home/project/app.view", updated["/home/project/app.view"], 4)
	assertEditDiagnostics(t, client, "/home/project/app.view")
	msg, actions, ok = client.SendRequest(t, lsproto.TextDocumentCodeActionInfo, &lsproto.CodeActionParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI("/home/project/app.view")}, Context: &lsproto.CodeActionContext{Diagnostics: []*lsproto.Diagnostic{}, Only: new([]lsproto.CodeActionKind{lsproto.CodeActionKindSourceOrganizeImports})}})
	assert.Assert(t, ok && msg.AsResponse().Error == nil)
	assert.Assert(t, actions.CommandOrCodeActionArray == nil || len(*actions.CommandOrCodeActionArray) == 0, "no-op import action escaped")
}
