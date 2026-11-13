package verb

import (
	"context"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// PinShowInput names a run and the config-relative path it pinned.
type PinShowInput struct {
	Run  string
	Path string
}

// PinShowResult is the bytes the run pinned at Path, exactly as pinned.
type PinShowResult struct {
	Run  string
	Path string
	Body string
}

// PinShow reads the bytes run in.Run pinned at in.Path when it activated. A
// path the run never pinned, or a file edited since activation, is refused
// rather than read off disk. It is a read: no lease, no lock, no event, and an
// identity no project holds is never registered.
func PinShow(ctx context.Context, env Env, in PinShowInput) (PinShowResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return PinShowResult{}, err
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return PinShowResult{}, newError(err, output.ErrValidation)
	}
	body, err := engine.PinContentStore(env.Store, runID, in.Path)
	if err != nil {
		return PinShowResult{}, engineError(err)
	}
	return PinShowResult{Run: model.FormatRunID(runID), Path: in.Path, Body: body}, nil
}
