package lsp_test

import (
	"strings"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/json"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"gotest.tools/v3/assert"
)

func TestDerivedRenameDoesNotChangeNavigation(t *testing.T) {
	t.Parallel()
	var baseline []string
	for _, mode := range []string{"derived-disabled", "derived"} {
		files := editTestFiles()
		client := newEditLSPClient(t, files, &rpcEditMapper{mode: mode}, lsproto.PositionEncodingKindUTF16)
		const name = "/home/project/app.view"
		openEditDocument(t, client, name, files[name], 1)
		doc := lsproto.TextDocumentIdentifier{Uri: editTestURI(name)}
		position := editTestPosition(name, files[name], strings.Index(files[name], "save-item")+3, lsproto.PositionEncodingKindUTF16)
		var responses []string
		record := func(value any) {
			encoded, err := json.Marshal(value)
			assert.NilError(t, err)
			responses = append(responses, string(encoded))
		}
		msg, hover, ok := client.SendRequest(t, lsproto.TextDocumentHoverInfo, &lsproto.HoverParams{TextDocument: doc, Position: position})
		assert.Assert(t, ok && msg.AsResponse().Error == nil)
		record(hover)
		msg, definition, ok := client.SendRequest(t, lsproto.TextDocumentDefinitionInfo, &lsproto.DefinitionParams{TextDocument: doc, Position: position})
		assert.Assert(t, ok && msg.AsResponse().Error == nil)
		record(definition)
		msg, references, ok := client.SendRequest(t, lsproto.TextDocumentReferencesInfo, &lsproto.ReferenceParams{TextDocument: doc, Position: position, Context: &lsproto.ReferenceContext{IncludeDeclaration: true}})
		assert.Assert(t, ok && msg.AsResponse().Error == nil)
		record(references)
		if baseline == nil {
			baseline = responses
		} else {
			assert.DeepEqual(t, responses, baseline)
		}
	}
}
