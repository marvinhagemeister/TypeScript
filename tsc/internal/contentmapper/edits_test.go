package contentmapper_test

import (
	"context"
	"errors"
	"testing"

	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/json"
	"github.com/microsoft/TypeScript/tsc/internal/locale"
	"gotest.tools/v3/assert"
)

func TestRenameInputWirePresence(t *testing.T) {
	t.Parallel()
	for _, name := range []*string{nil, new(""), new("save-foo")} {
		encoded, err := json.Marshal(contentmapper.PrepareRenameParams{NewName: name})
		assert.NilError(t, err)
		var decoded contentmapper.PrepareRenameParams
		assert.NilError(t, json.Unmarshal(encoded, &decoded))
		if name == nil {
			assert.Assert(t, decoded.NewName == nil)
		} else {
			assert.Assert(t, decoded.NewName != nil, "execution input was omitted: %s", encoded)
			assert.Equal(t, *decoded.NewName, *name)
		}
	}
}

type editCapabilityMapper struct {
	fakeMapper
	capabilities contentmapper.EditProjectionCapabilities
}

func (editCapabilityMapper) handlesProjects() {}
func (m editCapabilityMapper) HandleRequest(ctx context.Context, method string, params json.Value) (any, error) {
	switch method {
	case contentmapper.MethodOpenProject:
		return contentmapper.OpenProjectResult{EditProjection: &m.capabilities}, nil
	case contentmapper.MethodCloseProject:
		return nil, nil
	default:
		return m.fakeMapper.HandleRequest(ctx, method, params)
	}
}

func TestEditingSubCapabilitiesRequireRename(t *testing.T) {
	t.Parallel()
	for _, subCapability := range []string{"renameInput", "derivedRename"} {
		for _, rename := range []bool{false, true} {
			caps := contentmapper.EditProjectionCapabilities{Version: contentmapper.EditProjectionVersion, RenameInput: subCapability == "renameInput", DerivedRename: subCapability == "derivedRename", Rename: rename}
			host := contentmapper.NewHost(t.Context(), &fakeSpawner{handler: editCapabilityMapper{capabilities: caps}}, locale.Default)
			defer host.Close()
			mapper := &contentmapper.Mapper{Name: "mapper", Version: "1.0.0", Exec: []string{"mapper"}}
			project := host.Project(contentmapper.ProjectSpec{Mappers: []*contentmapper.Mapper{mapper}, CompilerOptions: &core.CompilerOptions{}})
			defer project.Close()
			_, err := project.Transform(mapper, contentmapper.Request{FileName: "/repo/file.ext", Content: "x"})
			if rename {
				assert.NilError(t, err)
				assert.Equal(t, project.(contentmapper.EditingProject).EditCapabilities(mapper), caps)
			} else {
				projectError, ok := errors.AsType[*contentmapper.ProjectError](err)
				assert.Assert(t, ok, "expected malformed capabilities: %v", err)
				assert.Equal(t, projectError.Kind, contentmapper.ProjectErrorKindMalformedResponse)
			}
		}
	}
}
