package lsp_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/lsp/lsproto"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
	"github.com/microsoft/TypeScript/tsc/internal/testutil/contentmappertest"
	"gotest.tools/v3/assert"
)

// Reduced from Vue's template event codegen: the JSDoc reference preserves event identity;
// the contextual onName property is derived, but deliberately NOT a semantic rename occurrence.
func derivedListenerTransform(original string, independent bool) contentmapper.Result {
	base := contentmappertest.ListenerTransform(false, false)(original)
	var text strings.Builder
	text.WriteString("import { child } from \"./child\";\ntype Props = { [K in keyof typeof child as `on${Capitalize<K>}`]?: number };\n")
	var spans []spanmap.Segment
	for i, segment := range base.Mappings.Segments() {
		name := base.Text[segment.VirtualStart:segment.VirtualEnd]
		text.WriteString(fmt.Sprintf("const event%d: Props = {\n/** @type {typeof child.", i))
		span := segment
		span.VirtualStart = core.TextPos(text.Len())
		text.WriteString(name)
		span.VirtualEnd = core.TextPos(text.Len())
		spans = append(spans, span)
		text.WriteString("} */\n")
		span.VirtualStart = core.TextPos(text.Len())
		text.WriteString("on" + strings.ToUpper(name[:1]) + name[1:])
		span.VirtualEnd = core.TextPos(text.Len())
		span.Features = spanmap.FeatureAll &^ spanmap.FeatureRename
		spans = append(spans, span)
		text.WriteString(fmt.Sprintf(": 1 }; void event%d;\n", i))
	}
	if independent && len(spans) != 0 {
		text.WriteString("const unrelated = { independent: 1 }; unrelated.")
		span := spans[0]
		span.VirtualStart = core.TextPos(text.Len())
		text.WriteString("independent")
		span.VirtualEnd = core.TextPos(text.Len())
		span.Features = spanmap.FeatureNone
		spans = append(spans, span)
		text.WriteString(";\n")
	}
	return contentmapper.Result{Text: text.String(), VirtualExtension: ".ts", Mappings: spanmap.New(spans)}
}

func accountDerivedFixture(params contentmapper.ProjectEditsParams, result *contentmapper.ProjectEditsResult) error {
	for _, effect := range params.Effects {
		var projection contentmapper.EditProjection
		for _, candidate := range params.Projections {
			if candidate.ID == effect.Projection {
				projection = candidate
			}
		}
		if effect.Start < 0 || effect.End > len(projection.Text) || !strings.HasPrefix(projection.Text[effect.Start:effect.End], "on") {
			return errors.New("fixture rejects independent projection")
		}
		accounted := false
		for i := range result.Results {
			for _, input := range effect.Inputs {
				if slices.Contains(result.Results[i].Inputs, input) {
					result.Results[i].DerivedEffects = append(result.Results[i].DerivedEffects, effect.ID)
					accounted = true
					break
				}
			}
			if accounted {
				break
			}
		}
		if !accounted {
			return errors.New("unrooted fixture effect")
		}
	}
	return nil
}

func TestLSPDerivedRenameProcess(t *testing.T) {
	t.Parallel()
	for _, encoding := range []lsproto.PositionEncodingKind{lsproto.PositionEncodingKindUTF8, lsproto.PositionEncodingKindUTF16} {
		for _, origin := range []string{"declaration", "camel", "kebab"} {
			t.Run(string(encoding)+"/"+origin, func(t *testing.T) {
				t.Parallel()
				files := editTestFiles()
				files["/home/project/node_modules/mapper/package.json"] = `{"name":"mapper","version":"1.0.0","typescript":{"contentMapper":{"exec":["projection-mapper","--derived"]}}}`
				client := newEditLSPClient(t, files, nil, encoding)
				name, spelling := "/home/project/child.ts", "saveItem"
				if origin != "declaration" {
					name = "/home/project/app.view"
					if origin == "kebab" {
						spelling = "save-item"
					}
				}
				openEditDocument(t, client, name, files[name], 1)
				pos := editTestPosition(name, files[name], strings.Index(files[name], spelling)+2, encoding)
				msg, prepared, ok := client.SendRequest(t, lsproto.TextDocumentPrepareRenameInfo, &lsproto.PrepareRenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI(name)}, Position: pos})
				assert.Assert(t, ok && msg.AsResponse().Error == nil, "%v", msg.AsResponse().Error)
				assert.Equal(t, prepared.PrepareRenamePlaceholder.Placeholder, spelling)
				msg, response, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, &lsproto.RenameParams{TextDocument: lsproto.TextDocumentIdentifier{Uri: editTestURI(name)}, Position: pos, NewName: "nextItem"})
				assert.Assert(t, ok && msg.AsResponse().Error == nil, "%v", msg.AsResponse().Error)
				updated := applyLSPEdit(t, files, response.WorkspaceEdit, encoding)
				assert.Equal(t, updated["/home/project/app.view"], strings.ReplaceAll(strings.ReplaceAll(projectionComponent, "saveItem", "nextItem"), "save-item", "next-item"))
				for _, changed := range []string{"/home/project/child.ts", "/home/project/app.view"} {
					if changed == name {
						changeEditDocument(t, client, changed, updated[changed], 2)
					} else {
						openEditDocument(t, client, changed, updated[changed], 2)
					}
					assertEditDiagnostics(t, client, changed)
				}
			})
		}
	}
}

func TestLSPDerivedRenameRequiresCompleteNegotiatedAccounting(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"derived-disabled", "derived-missing", "derived-foreign", "derived-duplicate", "derived-independent"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			files := editTestFiles()
			mapper := &rpcEditMapper{mode: mode}
			mapper.projectResult = func(params contentmapper.ProjectEditsParams) (contentmapper.ProjectEditsResult, error) {
				result, err := projectEditFixture(params)
				if err != nil {
					return result, err
				}
				if mode != "derived-missing" {
					err = accountDerivedFixture(params, &result)
				}
				if mode == "derived-foreign" {
					result.Results[0].DerivedEffects = []int{9999}
				}
				if mode == "derived-duplicate" {
					result.Results[0].DerivedEffects = append(result.Results[0].DerivedEffects, result.Results[0].DerivedEffects[0])
				}
				return result, err
			}
			client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
			name := "/home/project/child.ts"
			openEditDocument(t, client, name, files[name], 1)
			msg, response, _ := client.SendRequest(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, name))
			assert.Assert(t, msg.AsResponse().Error != nil)
			expected := map[string]string{"derived-disabled": "uncovered rename projection", "derived-missing": "incomplete derived effect coverage", "derived-foreign": "unknown or duplicate derived effect", "derived-duplicate": "unknown or duplicate derived effect", "derived-independent": "fixture rejects independent projection"}
			assert.Assert(t, strings.Contains(msg.AsResponse().Error.Message, expected[mode]), "%v", msg.AsResponse().Error)
			assert.Assert(t, response.WorkspaceEdit == nil)
		})
	}
}

func TestLSPRenameBatchesUseProjectContext(t *testing.T) {
	t.Parallel()
	files := editSolutionFiles()
	var mu sync.Mutex
	handles := map[string]string{}
	calls := map[string]bool{}
	identities := map[string]bool{}
	mapper := &rpcEditMapper{mode: "derived", openProject: func(params contentmapper.OpenProjectParams) {
		mu.Lock()
		defer mu.Unlock()
		handles[params.ProjectHandle] = params.ConfigFileName
	}, onProject: func(params contentmapper.ProjectEditsParams) {
		mu.Lock()
		defer mu.Unlock()
		config := handles[params.ProjectHandle]
		assert.Assert(t, config != "")
		assert.Assert(t, !calls[params.ProjectHandle], "one batch per project context")
		calls[params.ProjectHandle] = true
		for _, projection := range params.Projections {
			identities[projection.TransformIdentity] = true
		}
		for _, doc := range params.Documents {
			assert.Equal(t, doc.FileName[:strings.LastIndex(doc.FileName, "/")], config[:strings.LastIndex(config, "/")])
		}
	}}
	client := newEditLSPClient(t, files, mapper, lsproto.PositionEncodingKindUTF16)
	const name = "/home/project/child.ts"
	openEditDocument(t, client, "/home/project/first/app.view", projectionComponent, 1)
	openEditDocument(t, client, name, files[name], 1)
	msg, response, ok := client.SendRequest(t, lsproto.TextDocumentRenameInfo, editRenameParams(files, name))
	assert.Assert(t, ok && msg.AsResponse().Error == nil, "%v", msg.AsResponse().Error)
	updated := applyLSPEdit(t, files, response.WorkspaceEdit, lsproto.PositionEncodingKindUTF16)
	for _, project := range []string{"first", "second"} {
		assert.Assert(t, strings.Contains(updated["/home/project/"+project+"/app.view"], "next-item"))
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, len(calls), 2)
	assert.Equal(t, len(identities), 1, "equal transform identities must not merge editing contexts")
}
