package contentmapper

import (
	"context"
	"errors"
	"time"

	"github.com/microsoft/TypeScript/tsc/internal/ipc"
	"github.com/microsoft/TypeScript/tsc/internal/json"
)

const (
	MethodPrepareRename   = "prepareRename"
	MethodProjectEdits    = "projectEdits"
	EditProjectionVersion = 1
	editRequestTimeout    = 5 * time.Second
)

// EditProjectionCapabilities is an optional, versioned openProject result. Omission preserves the
// original exact-only LSP behavior. Editing uses UTF-8 byte offsets independently of transform encoding.
type EditProjectionCapabilities struct {
	Version         int  `json:"version"`
	Rename          bool `json:"rename,omitempty"`
	OrganizeImports bool `json:"organizeImports,omitempty"`
	// RenameInput opts into authored placeholders and execution-time input normalization. Requires Rename.
	RenameInput bool `json:"renameInput,omitzero"`
	// DerivedRename enables explicit accounting of collateral generated rename effects.
	DerivedRename bool `json:"derivedRename,omitzero"`
}

// EditingProject is optional so compiler-only hosts need not implement editing RPCs.
type EditingProject interface {
	EditCapabilities(mapper *Mapper) EditProjectionCapabilities
	PrepareRename(ctx context.Context, mapper *Mapper, params PrepareRenameParams) (PrepareRenameResult, error)
	ProjectEdits(ctx context.Context, mapper *Mapper, params ProjectEditsParams) (ProjectEditsResult, error)
}

type PrepareRenameParams struct {
	ProjectHandle    string           `json:"projectHandle"`
	Snapshot         string           `json:"snapshot"`
	PositionEncoding PositionEncoding `json:"positionEncoding"`
	FileName         string           `json:"fileName"`
	Content          string           `json:"content"`
	Start            int              `json:"start"`
	End              int              `json:"end"`
	Name             string           `json:"name"`
	// NewName is the raw authored input, present only during execution with RenameInput enabled.
	NewName *string `json:"newName,omitzero"`
}

type PrepareRenameResult struct {
	Snapshot  string `json:"snapshot"`
	CanRename bool   `json:"canRename"`
	Message   string `json:"message,omitempty"`
	// Accepted RenameInput requests require Placeholder; execution also requires NormalizedName.
	Placeholder    *string `json:"placeholder,omitzero"`
	NormalizedName *string `json:"normalizedName,omitzero"`
}

type EditDocument struct {
	ID       int    `json:"id"`
	FileName string `json:"fileName"`
	Text     string `json:"text"`
	Version  *int32 `json:"version"`
}

type EditProjection struct {
	ID                int        `json:"id"`
	Document          int        `json:"document"`
	FileName          string     `json:"fileName"`
	Text              string     `json:"text"`
	TransformIdentity string     `json:"transformIdentity"`
	Mappings          json.Value `json:"mappings"`
}

type GeneratedEdit struct {
	ID         int    `json:"id"`
	Projection int    `json:"projection"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	NewText    string `json:"newText"`
}

// RenameEffect is geometry affected by a semantic rename, not an additional semantic edit.
type RenameEffect struct {
	ID         int   `json:"id"`
	Projection int   `json:"projection"`
	Start      int   `json:"start"`
	End        int   `json:"end"`
	Inputs     []int `json:"inputs"`
}

type ProjectEditsParams struct {
	ProjectHandle    string           `json:"projectHandle"`
	Snapshot         string           `json:"snapshot"`
	PositionEncoding PositionEncoding `json:"positionEncoding"`
	Operation        string           `json:"operation"`
	NewName          string           `json:"newName,omitempty"`
	ImportAction     string           `json:"importAction,omitempty"`
	Documents        []EditDocument   `json:"documents"`
	Projections      []EditProjection `json:"projections"`
	Edits            []GeneratedEdit  `json:"edits"`
	Effects          []RenameEffect   `json:"effects,omitempty"`
}

type AuthoredEdit struct {
	Document int    `json:"document"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	NewText  string `json:"newText"`
}

type EditCoverage struct {
	DerivedEffects []int          `json:"derivedEffects,omitempty"`
	Inputs         []int          `json:"inputs"`
	Edits          []AuthoredEdit `json:"edits,omitempty"`
	GeneratedOnly  bool           `json:"generatedOnly,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

type ProjectEditsResult struct {
	Snapshot string         `json:"snapshot"`
	Results  []EditCoverage `json:"results"`
}

func (p *projectLease) EditCapabilities(mapper *Mapper) EditProjectionCapabilities {
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	if entry := p.host.projects[p.entries[mapper]]; entry != nil && entry.opened {
		return entry.editProjection
	}
	return EditProjectionCapabilities{}
}

func (p *projectLease) editConnection(mapper *Mapper, rename bool) (*ipc.AsyncConn, string, error) {
	p.host.lifecycleMu.RLock()
	defer p.host.lifecycleMu.RUnlock()
	p.host.mu.Lock()
	defer p.host.mu.Unlock()
	entry := p.host.projects[p.entries[mapper]]
	if entry == nil || !entry.opened || entry.editProjection.Version != EditProjectionVersion ||
		rename && !entry.editProjection.Rename || !rename && !entry.editProjection.OrganizeImports {
		return nil, "", errors.New("content mapper has not enabled this edit operation")
	}
	connection := p.host.conns[mapper.Identity()]
	if connection == nil || connection.conn == nil {
		return nil, "", errors.New("content mapper connection is unavailable")
	}
	conn, ok := connection.conn.(*ipc.AsyncConn)
	if !ok {
		return nil, "", errors.New("content mapper editing requires an asynchronous connection")
	}
	return conn, entry.projectHandle, nil
}

func (p *projectLease) PrepareRename(ctx context.Context, mapper *Mapper, params PrepareRenameParams) (PrepareRenameResult, error) {
	conn, handle, err := p.editConnection(mapper, true)
	if err != nil {
		return PrepareRenameResult{}, err
	}
	params.ProjectHandle, params.PositionEncoding = handle, PositionEncodingUTF8
	ctx, cancel := context.WithTimeout(ctx, editRequestTimeout)
	defer cancel()
	// No host, project, or checker lock is held while awaiting third-party code.
	raw, err := conn.CallWithWriteCancellation(ctx, MethodPrepareRename, params)
	if err != nil {
		return PrepareRenameResult{}, err
	}
	var result PrepareRenameResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return PrepareRenameResult{}, err
	}
	if result.Snapshot != params.Snapshot {
		return PrepareRenameResult{}, errors.New("content mapper returned stale rename preparation")
	}
	return result, nil
}

func (p *projectLease) ProjectEdits(ctx context.Context, mapper *Mapper, params ProjectEditsParams) (ProjectEditsResult, error) {
	if params.Operation != "rename" && params.Operation != "organizeImports" {
		return ProjectEditsResult{}, errors.New("unsupported edit projection operation")
	}
	conn, handle, err := p.editConnection(mapper, params.Operation == "rename")
	if err != nil {
		return ProjectEditsResult{}, err
	}
	params.ProjectHandle, params.PositionEncoding = handle, PositionEncodingUTF8
	ctx, cancel := context.WithTimeout(ctx, editRequestTimeout)
	defer cancel()
	raw, err := conn.CallWithWriteCancellation(ctx, MethodProjectEdits, params)
	if err != nil {
		return ProjectEditsResult{}, err
	}
	var result ProjectEditsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ProjectEditsResult{}, err
	}
	return result, nil
}
