package lsp_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"gotest.tools/v3/assert"
)

func TestLSPProjectedAuthoredRenameProcess(t *testing.T) {
	t.Parallel()
	for _, encoding := range []lsproto.PositionEncodingKind{lsproto.PositionEncodingKindUTF8, lsproto.PositionEncodingKindUTF16} {
		for _, prepare := range []bool{false, true} {
			for _, input := range []string{"save-foo", "@save-foo"} {
				t.Run(fmt.Sprintf("%s/prepare=%t/%s", encoding, prepare, input), func(t *testing.T) {
					t.Parallel()
					files := editTestFiles()
					const name = "/home/project/app.view"
					// Keep a non-BMP character on the trigger's line to exercise UTF-16/byte conversion.
					files[name] = strings.Replace(projectionComponent, "\n<Child", " <Child", 1)
					client := newEditLSPClient(t, files, nil, encoding)
					openEditDocument(t, client, name, files[name], 1)
					start := strings.Index(files[name], "save-item")
					params := &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI(name)}, Position: editTestPosition(name, files[name], start+3, encoding), NewName: input}
					if prepare {
						msg, info, ok := client.SendRequest(t, lsproto.TextDocumentPrepareRenameInfo, &lsproto.PrepareRenameParams{TextDocument: params.TextDocument, Position: params.Position})
						assert.Assert(t, ok && msg.AsResponse().Error == nil, "%v", msg.AsResponse().Error)
						assert.Assert(t, info.PrepareRenamePlaceholder != nil)
						assert.Equal(t, info.PrepareRenamePlaceholder.Placeholder, "save-item")
						assert.Equal(t, info.PrepareRenamePlaceholder.Range.Start, editTestPosition(name, files[name], start, encoding))
						assert.Equal(t, info.PrepareRenamePlaceholder.Range.End, editTestPosition(name, files[name], start+len("save-item"), encoding))
					}
					msg, response, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, params)
					assert.Assert(t, ok && msg.AsResponse().Error == nil, "%v", msg.AsResponse().Error)
					updated := applyLSPEdit(t, files, response.WorkspaceEdit, encoding)
					assert.Equal(t, updated["/home/project/child.ts"], "export const child = { saveFoo: 1 };\n")
					assert.Equal(t, updated[name], strings.ReplaceAll(strings.ReplaceAll(files[name], "saveItem", "saveFoo"), "save-item", "save-foo"))
					changeEditDocument(t, client, name, updated[name], 2)
					openEditDocument(t, client, "/home/project/child.ts", updated["/home/project/child.ts"], 2)
					assertEditDiagnostics(t, client, name)
					assertEditDiagnostics(t, client, "/home/project/child.ts")
				})
			}
		}
	}
}

func acceptedRenameInput(params contentmapper.PrepareRenameParams) contentmapper.PrepareRenameResult {
	return contentmapper.PrepareRenameResult{Snapshot: params.Snapshot, CanRename: true, Placeholder: new("save-item"), NormalizedName: new("persistRecord")}
}

func TestLSPProjectedRenameMapperOwnsInputPolicy(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	const name = "/home/project/app.view"
	files["/home/project/other.view"] = projectionComponent
	var normalized, projected atomic.Int32
	mapper := &rpcEditMapper{
		prepareResult: func(params contentmapper.PrepareRenameParams) (contentmapper.PrepareRenameResult, error) {
			assert.Equal(t, params.Name, "saveItem")
			result := acceptedRenameInput(params)
			// The host must honor the mapper's placeholder, not derive it by case conversion.
			result.Placeholder = new("event:save-item")
			if params.NewName != nil {
				normalized.Add(1)
				assert.Equal(t, *params.NewName, "otherName") // Already a valid identifier: still normalize.
				assert.Assert(t, strings.Contains(params.Content, "changed after preparation"))
			}
			return result, nil
		},
		onProject: func(params contentmapper.ProjectEditsParams) {
			projected.Add(1)
			assert.Equal(t, params.NewName, "persistRecord")
			assert.Equal(t, len(params.Documents), 2)
			for _, edit := range params.Edits {
				assert.Equal(t, edit.NewText, "persistRecord")
			}
		},
	}
	client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
	openEditDocument(t, client, name, files[name], 1)
	params := editRenameParams(files, name)
	msg, prepared, ok := client.SendRequest(t, lsproto.TextDocumentPrepareRenameInfo, &lsproto.PrepareRenameParams{TextDocument: params.TextDocument, Position: params.Position})
	assert.Assert(t, ok && msg.AsResponse().Error == nil)
	assert.Equal(t, prepared.PrepareRenamePlaceholder.Placeholder, "event:save-item")
	// Execution must use current authored context, not a cached preparation result.
	files[name] += "<!-- changed after preparation -->"
	changeEditDocument(t, client, name, files[name], 2)
	params.NewName = "otherName"
	msg, response, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, params)
	assert.Assert(t, ok && msg.AsResponse().Error == nil, "%v", msg.AsResponse().Error)
	updated := applyLSPEdit(t, files, response.WorkspaceEdit, lsproto.PositionEncodingKindUTF16)
	assert.Equal(t, updated["/home/project/child.ts"], "export const child = { persistRecord: 1 };\n")
	assert.Assert(t, strings.Contains(updated[name], "@persist-record"))
	assert.Assert(t, strings.Contains(updated["/home/project/other.view"], "@persist-record"))
	assert.Equal(t, normalized.Load(), int32(1))
	assert.Equal(t, projected.Load(), int32(1))
	assert.Equal(t, params.NewName, "otherName")
}

func TestLSPProjectedRenameCanonicalInputCompatibility(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	const name = "/home/project/app.view"
	mapper := &rpcEditMapper{mode: "canonical-input", prepareResult: func(params contentmapper.PrepareRenameParams) (contentmapper.PrepareRenameResult, error) {
		assert.Assert(t, params.NewName == nil, "unnegotiated input was sent")
		// Unnegotiated optional fields must not change the canonical-only contract.
		return acceptedRenameInput(params), nil
	}}
	client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
	openEditDocument(t, client, name, files[name], 1)
	params := editRenameParams(files, name)
	msg, prepared, ok := client.SendRequest(t, lsproto.TextDocumentPrepareRenameInfo, &lsproto.PrepareRenameParams{TextDocument: params.TextDocument, Position: params.Position})
	assert.Assert(t, ok && msg.AsResponse().Error == nil)
	assert.Equal(t, prepared.PrepareRenamePlaceholder.Placeholder, "saveItem")
	msg, response, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, params)
	assert.Assert(t, ok && msg.AsResponse().Error == nil)
	updated := applyLSPEdit(t, files, response.WorkspaceEdit, lsproto.PositionEncodingKindUTF16)
	assert.Equal(t, updated["/home/project/child.ts"], "export const child = { nextItem: 1 };\n")
	params.NewName = "next-item"
	msg, response, _ = client.SendRequest(t, lsproto.TextDocumentRenameInfo, params)
	assert.ErrorContains(t, msg.AsResponse().Error, "expected a canonical identifier")
	assert.Assert(t, response.WorkspaceEdit == nil)
}

func TestLSPProjectedRenameDoesNotNormalizeAtDestinations(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	var prepared, projected atomic.Int32
	mapper := &rpcEditMapper{
		onPrepare: func(contentmapper.PrepareRenameParams) { prepared.Add(1) },
		onProject: func(params contentmapper.ProjectEditsParams) {
			projected.Add(1)
			assert.Equal(t, params.NewName, "nextItem")
		},
	}
	client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
	openEditDocument(t, client, "/home/project/child.ts", files["/home/project/child.ts"], 1)
	params := editRenameParams(files, "/home/project/child.ts")
	msg, response, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, params)
	assert.Assert(t, ok && msg.AsResponse().Error == nil)
	assert.Assert(t, response.WorkspaceEdit != nil)
	params.NewName = "next-item"
	msg, response, _ = client.SendRequest(t, lsproto.TextDocumentRenameInfo, params)
	assert.Assert(t, msg.AsResponse().Error != nil)
	assert.Assert(t, response.WorkspaceEdit == nil)
	assert.Equal(t, prepared.Load(), int32(0))
	assert.Equal(t, projected.Load(), int32(1))
}

func TestLSPProjectedRenameRejectsInvalidNormalization(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*contentmapper.PrepareRenameResult)
	}{
		{"missing-placeholder", func(r *contentmapper.PrepareRenameResult) { r.Placeholder = nil }},
		{"empty-placeholder", func(r *contentmapper.PrepareRenameResult) { r.Placeholder = new("") }},
		{"missing-normalized", func(r *contentmapper.PrepareRenameResult) { r.NormalizedName = nil }},
		{"empty-normalized", func(r *contentmapper.PrepareRenameResult) { r.NormalizedName = new("") }},
		{"keyword", func(r *contentmapper.PrepareRenameResult) { r.NormalizedName = new("class") }},
		{"authored-not-canonical", func(r *contentmapper.PrepareRenameResult) { r.NormalizedName = new("save-foo") }},
		{"expression", func(r *contentmapper.PrepareRenameResult) { r.NormalizedName = new("a; evil()") }},
		{"rejected", func(r *contentmapper.PrepareRenameResult) { r.CanRename = false; r.Message = "unsupported input" }},
		{"stale", func(r *contentmapper.PrepareRenameResult) { r.Snapshot = "old" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			files := editTestFiles()
			var projected atomic.Int32
			mapper := &rpcEditMapper{
				prepareResult: func(params contentmapper.PrepareRenameParams) (contentmapper.PrepareRenameResult, error) {
					assert.Assert(t, params.NewName != nil)
					assert.Equal(t, *params.NewName, "nextItem") // Never silently fall back to this valid input.
					result := acceptedRenameInput(params)
					test.mutate(&result)
					return result, nil
				},
				onProject: func(contentmapper.ProjectEditsParams) { projected.Add(1) },
			}
			client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
			openEditDocument(t, client, "/home/project/app.view", projectionComponent, 1)
			msg, response, _ := client.SendRequest(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, "/home/project/app.view"))
			assert.Assert(t, msg.AsResponse().Error != nil)
			assert.Assert(t, response.WorkspaceEdit == nil)
			assert.Equal(t, projected.Load(), int32(0))
		})
	}
}

func TestLSPProjectedRenamePreservesEmptyInputOnWire(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	mapper := &rpcEditMapper{prepareResult: func(params contentmapper.PrepareRenameParams) (contentmapper.PrepareRenameResult, error) {
		assert.Assert(t, params.NewName != nil, "empty execution input became preparation")
		assert.Equal(t, *params.NewName, "")
		return contentmapper.PrepareRenameResult{}, errors.New("empty input rejected by mapper")
	}}
	client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
	openEditDocument(t, client, "/home/project/app.view", projectionComponent, 1)
	params := editRenameParams(files, "/home/project/app.view")
	params.NewName = ""
	msg, response, _ := client.SendRequest(t, lsproto.TextDocumentRenameInfo, params)
	assert.Assert(t, msg.AsResponse().Error != nil)
	assert.Assert(t, strings.Contains(msg.AsResponse().Error.Message, "empty input rejected by mapper"))
	assert.Assert(t, response.WorkspaceEdit == nil)
}

func TestLSPProjectedRenameNormalizationLifecycle(t *testing.T) {
	t.Parallel()
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancel), func(t *testing.T) {
			t.Parallel()
			files := editTestFiles()
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var projected atomic.Int32
			mapper := &rpcEditMapper{
				onPrepare: func(contentmapper.PrepareRenameParams) { entered <- struct{}{}; <-release },
				onProject: func(contentmapper.ProjectEditsParams) { projected.Add(1) },
			}
			client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
			openEditDocument(t, client, "/home/project/app.view", projectionComponent, 1)
			params := editRenameParams(files, "/home/project/app.view")
			params.NewName = "save-foo"
			id := client.NextID() + 1
			wait := client.SendRequestAsync(t, lsproto.TextDocumentRenameInfo, params)
			awaitEditRPC(t, entered)
			expectedCode := lsproto.ErrorCodeRequestCancelled
			if cancel {
				client.SendNotification(t, lsproto.CancelRequestInfo, &lsproto.CancelParams{Id: lsproto.IntegerOrString{Integer: new(id)}})
			} else {
				changeEditDocument(t, client, "/home/project/app.view", projectionComponent+"<!-- changed -->", 2)
				msg, _, ok := client.SendRequest(t, lsproto.TextDocumentHoverInfo, &lsproto.HoverParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI("/home/project/child.ts")}, Position: lsproto.Position{Character: 25}})
				assert.Assert(t, ok && msg.AsResponse().Error == nil)
				once.Do(func() { close(release) })
				expectedCode = lsproto.ErrorCodeContentModified
			}
			msg, response, _ := wait()
			assert.Assert(t, msg.AsResponse().Error != nil)
			assert.Equal(t, msg.AsResponse().Error.Code, int32(expectedCode))
			assert.Assert(t, response.WorkspaceEdit == nil)
			assert.Equal(t, projected.Load(), int32(0))
		})
	}
}

func TestLSPProjectedRenameNormalizationDeadline(t *testing.T) {
	t.Parallel()
	files := editTestFiles()
	client := newEditLSPClient(t, files, &rpcEditMapper{mode: "prepare-hang"}, lsproto.PositionEncodingKindUTF16)
	openEditDocument(t, client, "/home/project/app.view", projectionComponent, 1)
	params := editRenameParams(files, "/home/project/app.view")
	params.NewName = "save-foo"
	msg, response, _ := client.SendRequest(t, lsproto.TextDocumentRenameInfo, params)
	assert.Assert(t, msg.AsResponse().Error != nil)
	assert.Assert(t, strings.Contains(msg.AsResponse().Error.Message, "deadline exceeded"), "%v", msg.AsResponse().Error)
	assert.Assert(t, response.WorkspaceEdit == nil)
}

func TestLSPProjectedAuthoredRenameAcrossProjects(t *testing.T) {
	t.Parallel()
	files := editSolutionFiles()
	var normalizations atomic.Int32
	mapper := &rpcEditMapper{
		onPrepare: func(params contentmapper.PrepareRenameParams) {
			normalizations.Add(1)
			assert.Equal(t, params.FileName, "/home/project/first/app.view")
			assert.Assert(t, params.NewName != nil)
			assert.Equal(t, *params.NewName, "save-foo")
		},
		onProject: func(params contentmapper.ProjectEditsParams) { assert.Equal(t, params.NewName, "saveFoo") },
	}
	client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
	const name = "/home/project/first/app.view"
	openEditDocument(t, client, name, files[name], 1)
	params := editRenameParams(files, name)
	params.NewName = "save-foo"
	msg, response, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, params)
	assert.Assert(t, ok && msg.AsResponse().Error == nil, "%v", msg.AsResponse().Error)
	updated := applyLSPEdit(t, files, response.WorkspaceEdit, lsproto.PositionEncodingKindUTF16)
	assert.Equal(t, updated["/home/project/child.ts"], "export const child = { saveFoo: 1 };\n")
	for _, name := range []string{"first", "second"} {
		assert.Equal(t, updated["/home/project/"+name+"/app.view"], strings.ReplaceAll(strings.ReplaceAll(projectionComponent, "saveItem", "saveFoo"), "save-item", "save-foo"))
	}
	assert.Equal(t, normalizations.Load(), int32(1))
}

func TestLSPProjectedPrepareRejectsInvalidAuthoredPlaceholder(t *testing.T) {
	t.Parallel()
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			t.Parallel()
			files := editTestFiles()
			mapper := &rpcEditMapper{prepareResult: func(params contentmapper.PrepareRenameParams) (contentmapper.PrepareRenameResult, error) {
				assert.Assert(t, params.NewName == nil)
				result := acceptedRenameInput(params)
				result.Placeholder = nil
				if empty {
					result.Placeholder = new("")
				}
				return result, nil
			}}
			client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
			openEditDocument(t, client, "/home/project/app.view", projectionComponent, 1)
			params := editRenameParams(files, "/home/project/app.view")
			msg, response, _ := client.SendRequest(t, lsproto.TextDocumentPrepareRenameInfo, &lsproto.PrepareRenameParams{TextDocument: params.TextDocument, Position: params.Position})
			assert.Assert(t, msg.AsResponse().Error != nil)
			assert.Assert(t, strings.Contains(msg.AsResponse().Error.Message, "placeholder"))
			assert.Assert(t, response.PrepareRenamePlaceholder == nil)
		})
	}
}
