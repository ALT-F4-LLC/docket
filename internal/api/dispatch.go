package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// DispatchAbandon retires a run's open manifest under the run's conductor
// capability.
func (s *Service) DispatchAbandon(ctx context.Context, req *connect.Request[docketv1.DispatchAbandonRequest]) (*connect.Response[docketv1.DispatchAbandonResponse], error) {
	return unary(ctx, s, req, decodeDispatchAbandon, verb.DispatchAbandon, encodeDispatchAbandon)
}

// DispatchBackfillUsage records usage a relay measured for a run's steps.
func (s *Service) DispatchBackfillUsage(ctx context.Context, req *connect.Request[docketv1.DispatchBackfillUsageRequest]) (*connect.Response[docketv1.DispatchBackfillUsageResponse], error) {
	return unary(ctx, s, req, decodeDispatchBackfillUsage, verb.DispatchBackfillUsage, encodeDispatchBackfillUsage)
}

// DispatchClose closes a run's open manifest, or with backfill rows
// reconciles it: back-fill, verify, then close.
func (s *Service) DispatchClose(ctx context.Context, req *connect.Request[docketv1.DispatchCloseRequest]) (*connect.Response[docketv1.DispatchCloseResponse], error) {
	return unary(ctx, s, req, decodeDispatchClose, verb.DispatchClose, encodeDispatchClose)
}

// DispatchExtend appends the steps that became ready since the open to a
// run's open manifest.
func (s *Service) DispatchExtend(ctx context.Context, req *connect.Request[docketv1.DispatchExtendRequest]) (*connect.Response[docketv1.DispatchExtendResponse], error) {
	return unary(ctx, s, req, decodeDispatchExtend, verb.DispatchExtend, encodeDispatchExtend)
}

// DispatchOpen records a run's ready set as its open manifest.
func (s *Service) DispatchOpen(ctx context.Context, req *connect.Request[docketv1.DispatchOpenRequest]) (*connect.Response[docketv1.DispatchOpenResponse], error) {
	return unary(ctx, s, req, decodeDispatchOpen, verb.DispatchOpen, encodeDispatchOpen)
}

// DispatchVerify compares a run's open manifest to its current ready set.
func (s *Service) DispatchVerify(ctx context.Context, req *connect.Request[docketv1.DispatchVerifyRequest]) (*connect.Response[docketv1.DispatchVerifyResponse], error) {
	return unary(ctx, s, req, decodeDispatchVerify, verb.DispatchVerify, encodeDispatchVerify)
}

// DispatchWaiveTarget records stale-target waivers under the run's conductor
// capability.
func (s *Service) DispatchWaiveTarget(ctx context.Context, req *connect.Request[docketv1.DispatchWaiveTargetRequest]) (*connect.Response[docketv1.DispatchWaiveTargetResponse], error) {
	return unary(ctx, s, req, decodeDispatchWaiveTarget, verb.DispatchWaiveTarget, encodeDispatchWaiveTarget)
}

// manifestOpen is the status of a manifest that open or extend answers with:
// both act only on the run's one open manifest.
const manifestOpen = "open"

func manifestMessage(dispatch, run, status string, rows []model.StepRow) *docketv1.Manifest {
	msg := &docketv1.Manifest{Id: dispatch, Run: run, Status: status}
	for _, row := range rows {
		msg.Rows = append(msg.Rows, stepRowMessage(row))
	}
	return msg
}

func decodeDispatchAbandon(req *docketv1.DispatchAbandonRequest) (verb.DispatchAbandonInput, error) {
	return verb.DispatchAbandonInput{
		Run: req.GetRun(), Reason: req.GetReason(), ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeDispatchAbandon(res verb.DispatchAbandonResult) *docketv1.DispatchAbandonResponse {
	a := res.Abandon
	if a == nil {
		return &docketv1.DispatchAbandonResponse{}
	}
	return &docketv1.DispatchAbandonResponse{Manifest: manifestMessage(a.Dispatch, a.Run, a.Status, nil)}
}

// dispatchUsageRow is the shape of the verb's back-fill row type. Row is
// inferred from the verb inputs, so this file names no engine type, and a
// change to the row's fields fails the build here.
type dispatchUsageRow = struct {
	Step     int
	Unit     string
	Quantity float64
	Metadata string
}

// setDispatchUsageRows sets *dst to rows. A dispatch's spend attaches to a
// step, never to a seat, so a row naming a voter is refused rather than
// recorded without it.
func setDispatchUsageRows[Row ~dispatchUsageRow](dst *[]Row, rows []*docketv1.UsageRow) error {
	batch := make([]Row, 0, len(rows))
	for i, r := range rows {
		if r.GetVoter() != "" {
			return fmt.Errorf("rows[%d].voter is set: dispatch usage attributes spend to a step, not a seat", i)
		}
		id, err := model.ParseStepID(r.GetStep())
		if err != nil {
			return fmt.Errorf("rows[%d].step %q: %w", i, r.GetStep(), err)
		}
		batch = append(batch, Row(dispatchUsageRow{Step: id, Unit: r.GetUnit(), Quantity: r.GetQuantity()}))
	}
	*dst = batch
	return nil
}

func decodeDispatchBackfillUsage(req *docketv1.DispatchBackfillUsageRequest) (verb.DispatchBackfillUsageInput, error) {
	in := verb.DispatchBackfillUsageInput{Run: req.GetRun(), Source: req.GetSource(), OnDuplicate: req.GetOnDuplicate()}
	if err := setDispatchUsageRows(&in.Rows, req.GetRows()); err != nil {
		return verb.DispatchBackfillUsageInput{}, err
	}
	return in, nil
}

func encodeDispatchBackfillUsage(res verb.DispatchBackfillUsageResult) *docketv1.DispatchBackfillUsageResponse {
	b := res.Backfill
	if b == nil {
		return &docketv1.DispatchBackfillUsageResponse{}
	}
	return &docketv1.DispatchBackfillUsageResponse{Recorded: int32(b.Written), Skipped: int32(len(b.Skipped))}
}

// errCloseStageOptions refuses back-fill options on a plain close: they apply
// to the reconcile's back-fill stage only, so honoring them would mean
// ignoring them.
var errCloseStageOptions = errors.New("source and on_duplicate apply only to a close that carries backfill_rows")

// decodeDispatchClose makes a close that carries backfill rows the reconcile.
// The verb reads the request's token only for an override on a bound run.
func decodeDispatchClose(req *docketv1.DispatchCloseRequest) (verb.DispatchCloseInput, error) {
	in := verb.DispatchCloseInput{
		Run: req.GetRun(), AcceptMissingUsage: req.GetAcceptMissingUsage(),
		SkipIntegrationCheck: req.GetSkipIntegrationCheck(), ConductorToken: conductorToken(req.GetToken()),
	}
	if len(req.GetBackfillRows()) == 0 {
		if req.GetSource() != "" || req.GetOnDuplicate() != "" {
			return verb.DispatchCloseInput{}, errCloseStageOptions
		}
		return in, nil
	}
	in.Reconcile, in.Source, in.OnDuplicate = true, req.GetSource(), req.GetOnDuplicate()
	if err := setDispatchUsageRows(&in.BackfillRows, req.GetBackfillRows()); err != nil {
		return verb.DispatchCloseInput{}, err
	}
	return in, nil
}

// encodeDispatchClose carries a reconcile refused at a later stage too: the
// back-fill that landed still reports its merged metadata, and the manifest
// is set only once the close stage ran.
func encodeDispatchClose(res verb.DispatchCloseResult) *docketv1.DispatchCloseResponse {
	msg := &docketv1.DispatchCloseResponse{}
	c := res.Close
	if r := res.Reconcile; r != nil {
		c = r.Close
		if r.Backfill != nil {
			msg.MetadataMerged = int32(r.Backfill.MetadataMerged)
		}
	}
	if c != nil {
		msg.Manifest = manifestMessage(c.Dispatch, c.Run, c.Status, nil)
		msg.MissingUsage = c.Accepted
	}
	return msg
}

func decodeDispatchExtend(req *docketv1.DispatchExtendRequest) (verb.DispatchExtendInput, error) {
	return verb.DispatchExtendInput{Run: req.GetRun()}, nil
}

// encodeDispatchExtend carries the appended rows in added and leaves the
// manifest's rows unset: the verb reports only what this call appended.
func encodeDispatchExtend(res verb.DispatchExtendResult) *docketv1.DispatchExtendResponse {
	e := res.Extension
	if e == nil {
		return &docketv1.DispatchExtendResponse{}
	}
	msg := &docketv1.DispatchExtendResponse{Manifest: manifestMessage(e.Dispatch, e.Run, manifestOpen, nil)}
	for _, row := range e.Rows {
		msg.Added = append(msg.Added, stepRowMessage(row))
	}
	return msg
}

func decodeDispatchOpen(req *docketv1.DispatchOpenRequest) (verb.DispatchOpenInput, error) {
	return verb.DispatchOpenInput{Run: req.GetRun(), Limit: int(req.GetLimit()), AckReap: req.GetAckReap()}, nil
}

// encodeDispatchOpen names each stale target by its step instance.
func encodeDispatchOpen(res verb.DispatchOpenResult) *docketv1.DispatchOpenResponse {
	m := res.Manifest
	if m == nil {
		return &docketv1.DispatchOpenResponse{}
	}
	msg := &docketv1.DispatchOpenResponse{Manifest: manifestMessage(m.Dispatch, m.Run, manifestOpen, m.Rows)}
	for _, p := range m.PinDrift {
		msg.PinDrift = append(msg.PinDrift, pinVerdictMessage(p))
	}
	for _, st := range m.StaleTargets {
		msg.StaleTargets = append(msg.StaleTargets, st.Instance)
	}
	return msg
}

func decodeDispatchVerify(req *docketv1.DispatchVerifyRequest) (verb.DispatchVerifyInput, error) {
	return verb.DispatchVerifyInput{Run: req.GetRun()}, nil
}

// encodeDispatchVerify answers a mismatch as match false with every row's
// verdict: the verb returns a mismatch as its result, not as a failure. A
// row that did not match carries its stored and recomputed renderings.
func encodeDispatchVerify(res verb.DispatchVerifyResult) *docketv1.DispatchVerifyResponse {
	v := res.Verify
	if v == nil {
		return &docketv1.DispatchVerifyResponse{}
	}
	msg := &docketv1.DispatchVerifyResponse{Match: v.Verified && res.Mismatch == nil}
	for _, row := range v.Rows {
		msg.Rows = append(msg.Rows, &docketv1.RowVerdict{
			Step: row.Step, Verdict: row.Verdict,
			ManifestRow: mustRenderedRow(row.Stored), CurrentRow: mustRenderedRow(row.Computed),
		})
	}
	return msg
}

// mustRenderedRow decodes a manifest row rendering, or returns nil for an
// absent one. The renderings are step rows the engine encoded, so a decode
// failure is a broken invariant and panics, as mustJSON does.
func mustRenderedRow(rendered string) *docketv1.StepRow {
	if rendered == "" {
		return nil
	}
	var row model.StepRow
	if err := json.Unmarshal([]byte(rendered), &row); err != nil {
		panic(fmt.Sprintf("decoding a manifest row: %v", err))
	}
	return stepRowMessage(row)
}

func decodeDispatchWaiveTarget(req *docketv1.DispatchWaiveTargetRequest) (verb.DispatchWaiveTargetInput, error) {
	return verb.DispatchWaiveTargetInput{
		Run: req.GetRun(), Steps: req.GetStep(), Target: req.GetTarget(), Note: req.GetNote(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

// encodeDispatchWaiveTarget names each waived step instance.
func encodeDispatchWaiveTarget(res verb.DispatchWaiveTargetResult) *docketv1.DispatchWaiveTargetResponse {
	msg := &docketv1.DispatchWaiveTargetResponse{}
	for _, w := range res.Waivers {
		msg.Waived = append(msg.Waived, w.Instance)
	}
	return msg
}
