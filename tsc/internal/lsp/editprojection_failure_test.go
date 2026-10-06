package lsp_test

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"gotest.tools/v3/assert"
)

func editRenameParams(files map[string]string, name string) *lsproto.RenameParams {
	spelling := "saveItem"
	if strings.HasSuffix(name, ".view") {
		spelling = "save-item"
	}
	return &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI(name)}, Position: editTestPosition(name, files[name], strings.Index(files[name], spelling)+2, lsproto.PositionEncodingKindUTF16), NewName: "nextItem"}
}

func TestLSPProjectedRenameRejectsWholeOperation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"failure", "stale", "missing", "unauthorized", "range", "overflow", "conflict", "reject-prepare"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			files := editTestFiles()
			client := newEditLSPClient(t, files, &rpcEditMapper{mode: mode}, lsproto.PositionEncodingKindUTF16)
			name := "/home/project/child.ts"
			if mode == "reject-prepare" {
				name = "/home/project/app.view"
			}
			openEditDocument(t, client, name, files[name], 1)
			// Execution must validate preparation even if the client never sends prepareRename.
			msg, response, _ := client.SendRequest(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, name))
			assert.Assert(t, msg.AsResponse().Error != nil)
			assert.Assert(t, response.WorkspaceEdit == nil, "partial edit escaped")
		})
	}
}

func TestLSPProjectedRenameLegacyCompatibility(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	var calls atomic.Int32
	mapper := &rpcEditMapper{mode: "legacy", onProject: func(contentmapper.ProjectEditsParams) { calls.Add(1) }, onPrepare: func(contentmapper.PrepareRenameParams) { calls.Add(1) }}
	client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
	name := "/home/project/app.view"
	openEditDocument(t, client, name, files[name], 1)
	params := editRenameParams(files, name)
	msg, _, _ := client.SendRequest(t, lsproto.TextDocumentPrepareRenameInfo, &lsproto.PrepareRenameParams{TextDocument: params.TextDocument, Position: params.Position})
	assert.Assert(t, msg.AsResponse().Error != nil, "legacy Atom rename unexpectedly became editable")
	msg, response, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, "/home/project/child.ts"))
	assert.Assert(t, ok && msg.AsResponse().Error == nil)
	assert.Assert(t, response.WorkspaceEdit != nil && response.WorkspaceEdit.Changes != nil)
	assert.Equal(t, len(*response.WorkspaceEdit.Changes), 1)
	assert.Equal(t, calls.Load(), int32(0))
}

func editSolutionFiles() map[string]string {
	files := editTestFiles()
	config := strings.Replace(files["/home/project/tsconfig.json"], `"strict":true`, `"strict":true,"composite":true,"rootDir":".."`, 1)
	files["/home/project/tsconfig.json"] = `{"files":[],"references":[{"path":"./first"},{"path":"./second"}]}`
	delete(files, "/home/project/app.view")
	for _, dir := range []string{"first", "second"} {
		files["/home/project/"+dir+"/tsconfig.json"] = config
		files["/home/project/"+dir+"/app.view"] = projectionComponent
		files["/home/project/"+dir+"/child.ts"] = "export { child } from '../child';"
	}
	return files
}

func TestLSPProjectedRenameCollectsProjects(t *testing.T) {
	t.Parallel()
	files := editSolutionFiles()
	client := newEditLSPClient(t, files, nil, lsproto.PositionEncodingKindUTF16)
	// Opening only one consumer must not hide the other project's mapped reference.
	openEditDocument(t, client, "/home/project/first/app.view", projectionComponent, 1)
	openEditDocument(t, client, "/home/project/child.ts", files["/home/project/child.ts"], 4)
	msg, response, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, "/home/project/child.ts"))
	assert.Assert(t, ok && msg.AsResponse().Error == nil, "cross-project rename: %v", msg.AsResponse().Error)
	updated := applyLSPEdit(t, files, response.WorkspaceEdit, lsproto.PositionEncodingKindUTF16)
	for _, name := range []string{"first", "second"} {
		assert.Equal(t, updated["/home/project/"+name+"/app.view"], strings.ReplaceAll(strings.ReplaceAll(projectionComponent, "saveItem", "nextItem"), "save-item", "next-item"))
	}
	assert.Equal(t, updated["/home/project/child.ts"], "export const child = { nextItem: 1 };\n")
}

func awaitEditRPC(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
		return
	case <-time.After(10 * time.Second):
		t.Fatal("mapper RPC was not reached")
	}
}

func TestLSPProjectedRenameRejectsConcurrentChange(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	mapper := &rpcEditMapper{onProject: func(contentmapper.ProjectEditsParams) { entered <- struct{}{}; <-release }}
	client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
	var once sync.Once
	defer once.Do(func() { close(release) })
	name := "/home/project/app.view"
	openEditDocument(t, client, name, files[name], 1)
	wait := client.SendRequestAsync(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, "/home/project/child.ts"))
	awaitEditRPC(t, entered)
	changeEditDocument(t, client, name, files[name]+"<!-- changed -->", 2)
	// A real semantic request acts as a notification barrier and proves mapper callbacks hold no
	// checker or host lock. It must complete while projection is still waiting on the mapper.
	msg, _, ok := client.SendRequest(t, lsproto.TextDocumentHoverInfo, &lsproto.HoverParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI("/home/project/child.ts")}, Position: lsproto.Position{Character: 25}})
	assert.Assert(t, ok && msg.AsResponse().Error == nil)
	once.Do(func() { close(release) })
	msg, response, _ := wait()
	assert.Assert(t, msg.AsResponse().Error != nil)
	assert.Equal(t, msg.AsResponse().Error.Code, int32(lsproto.ErrorCodeContentModified))
	assert.Assert(t, response.WorkspaceEdit == nil)
}

func TestLSPProjectedRenameCancellation(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	client := newEditLSPClient(t, files, &rpcEditMapper{onProject: func(contentmapper.ProjectEditsParams) { entered <- struct{}{}; <-release }}, lsproto.PositionEncodingKindUTF16)
	defer close(release)
	openEditDocument(t, client, "/home/project/child.ts", files["/home/project/child.ts"], 1)
	// SendRequestAsync consumes the next ID; reserve its value for the cancellation notification.
	id := client.NextID() + 1
	wait := client.SendRequestAsync(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, "/home/project/child.ts"))
	awaitEditRPC(t, entered)
	client.SendNotification(t, lsproto.CancelRequestInfo, &lsproto.CancelParams{Id: lsproto.IntegerOrString{Integer: new(id)}})
	msg, response, _ := wait()
	assert.Assert(t, msg.AsResponse().Error != nil)
	assert.Equal(t, msg.AsResponse().Error.Code, int32(lsproto.ErrorCodeRequestCancelled))
	assert.Assert(t, response.WorkspaceEdit == nil)
}

func TestLSPProjectedRenameRejectsDiskChanges(t *testing.T) {
	t.Parallel()
	for _, configChange := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed-source", true: "mapper-configuration"}[configChange], func(t *testing.T) {
			t.Parallel()
			files := editTestFiles()
			entered, release := make(chan struct{}, 1), make(chan struct{})
			mapper := &rpcEditMapper{onProject: func(contentmapper.ProjectEditsParams) { entered <- struct{}{}; <-release }}
			client, fs := newEditLSPClientWithFS(t, files, mapper, lsproto.PositionEncodingKindUTF16)
			var once sync.Once
			defer once.Do(func() { close(release) })
			openEditDocument(t, client, "/home/project/child.ts", files["/home/project/child.ts"], 1)
			wait := client.SendRequestAsync(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, "/home/project/child.ts"))
			awaitEditRPC(t, entered)
			if configChange {
				assert.NilError(t, fs.WriteFile("/home/project/tsconfig.json", strings.Replace(files["/home/project/tsconfig.json"], `"strict":true`, `"strict":false`, 1)))
				client.SendNotification(t, lsproto.WorkspaceDidChangeWatchedFilesInfo, &lsproto.DidChangeWatchedFilesParams{Changes: []*lsproto.FileEvent{{Uri: editTestURI("/home/project/tsconfig.json"), Type: lsproto.FileChangeTypeChanged}}})
				msg, _, ok := client.SendRequest(t, lsproto.TextDocumentHoverInfo, &lsproto.HoverParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI("/home/project/child.ts")}, Position: lsproto.Position{Character: 25}})
				assert.Assert(t, ok && msg.AsResponse().Error == nil)
			} else {
				// No watcher notification: the live disk check must catch this closed-file race.
				assert.NilError(t, fs.WriteFile("/home/project/app.view", projectionComponent+"<!-- changed on disk -->"))
			}
			once.Do(func() { close(release) })
			msg, response, _ := wait()
			assert.Assert(t, msg.AsResponse().Error != nil)
			assert.Equal(t, msg.AsResponse().Error.Code, int32(lsproto.ErrorCodeContentModified))
			assert.Assert(t, response.WorkspaceEdit == nil)
		})
	}
}

func TestLSPProjectedRenameProviderDeadline(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	client := newEditLSPClient(t, files, &rpcEditMapper{mode: "hang"}, lsproto.PositionEncodingKindUTF16)
	openEditDocument(t, client, "/home/project/child.ts", files["/home/project/child.ts"], 1)
	msg, response, _ := client.SendRequest(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, "/home/project/child.ts"))
	assert.Assert(t, msg.AsResponse().Error != nil)
	assert.Assert(t, strings.Contains(msg.AsResponse().Error.Message, "deadline exceeded"), "%v", msg.AsResponse().Error)
	assert.Assert(t, response.WorkspaceEdit == nil)
}

func TestLSPProjectedImportsClientCapabilityDoesNotDisableQuickFixes(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	client, _ := newEditLSPClientWithFS(t, files, &rpcEditMapper{}, lsproto.PositionEncodingKindUTF16, false)
	openEditDocument(t, client, "/home/project/app.view", projectionComponent, 1)
	request := &lsproto.CodeActionParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI("/home/project/app.view")}, Context: &lsproto.CodeActionContext{Diagnostics: []*lsproto.Diagnostic{}, Only: new([]lsproto.CodeActionKind{lsproto.CodeActionKindQuickFix})}}
	msg, _, ok := client.SendRequest(t, lsproto.TextDocumentCodeActionInfo, request)
	assert.Assert(t, ok && msg.AsResponse().Error == nil, "unrelated code action was disabled")
	request.Context.Only = new([]lsproto.CodeActionKind{lsproto.CodeActionKindSourceOrganizeImports})
	msg, actions, _ := client.SendRequest(t, lsproto.TextDocumentCodeActionInfo, request)
	assert.Assert(t, msg.AsResponse().Error != nil)
	assert.Assert(t, actions.CommandOrCodeActionArray == nil)
}
