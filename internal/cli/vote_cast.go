package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// voteCastResult is the JSON wire format for the vote cast response.
type voteCastResult struct {
	Vote           *model.Vote `json:"vote"`
	ProposalStatus string      `json:"proposal_status"`
	VotesCast      int         `json:"votes_cast"`
	VotesRequired  int         `json:"votes_required"`
	QuorumReached  bool        `json:"quorum_reached"`
	WeightedScore  *float64    `json:"weighted_score"`
}

// newVoteCastCmd builds a FRESH `vote cast` command, flags included. Both the
// package's registered voteCastCmd and the test suite build through this one
// factory (newRunActivateCmd and newImportCmd are the precedents): registering
// the flags in exactly one place means a test that builds its own instance
// parses the SAME flag set a shell invocation does, so a flag that stopped
// being registered — or a RunE that stopped reading one — fails a test instead
// of silently dropping every caster's value.
func newVoteCastCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cast <id>",
		Short: "Cast a vote on a proposal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := getWriter(cmd)
			conn := getDB(cmd)

			proposalID, err := model.ParseProposalID(args[0])
			if err != nil {
				return cmdErr(fmt.Errorf("invalid proposal ID: %w", err), output.ErrValidation)
			}

			voter, _ := cmd.Flags().GetString("voter")
			role, _ := cmd.Flags().GetString("role")
			verdict, _ := cmd.Flags().GetString("verdict")
			confidence, _ := cmd.Flags().GetFloat64("confidence")
			domainRelevance, _ := cmd.Flags().GetFloat64("domain-relevance")
			findings, _ := cmd.Flags().GetString("findings")
			findingsJSONRaw, _ := cmd.Flags().GetString("findings-json")
			summary, _ := cmd.Flags().GetString("summary")
			summaryFile, _ := cmd.Flags().GetString("summary-file")
			metadataRaw, _ := cmd.Flags().GetString("metadata")
			jsonMode, _ := jsonModeOf(cmd)

			// One source for the summary, decided before anything else runs.
			// Both flags write the same field, so a caster that passed both
			// has two rationales and no way to know which one landed.
			if cmd.Flags().Changed("summary") && cmd.Flags().Changed("summary-file") {
				return cmdErr(fmt.Errorf("--summary and --summary-file are mutually exclusive; pass one"), output.ErrValidation)
			}

			// Default voter to git user.name.
			if voter == "" {
				voter = config.DefaultAuthor()
			}

			// JSON mode: require all mandatory flags.
			// Note: --voter defaults to git user.name so it is effectively always
			// set; we skip a redundant voter=="" check here.
			if jsonMode {
				if verdict == "" {
					return cmdErr(fmt.Errorf("--verdict is required in JSON mode"), output.ErrValidation)
				}
				if !cmd.Flags().Changed("confidence") {
					return cmdErr(fmt.Errorf("--confidence is required in JSON mode"), output.ErrValidation)
				}
				if !cmd.Flags().Changed("domain-relevance") {
					return cmdErr(fmt.Errorf("--domain-relevance is required in JSON mode"), output.ErrValidation)
				}
			}

			// Determine if all required flags are present.
			allRequiredPresent := verdict != "" && cmd.Flags().Changed("confidence") && cmd.Flags().Changed("domain-relevance")

			// Interactive form when not in JSON mode and required flags are missing.
			if !jsonMode && !allRequiredPresent {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					var missing []string
					if verdict == "" {
						missing = append(missing, "--verdict")
					}
					if !cmd.Flags().Changed("confidence") {
						missing = append(missing, "--confidence")
					}
					if !cmd.Flags().Changed("domain-relevance") {
						missing = append(missing, "--domain-relevance")
					}
					return cmdErr(fmt.Errorf("non-interactive environment detected; provide all required flags: %s", strings.Join(missing, ", ")), output.ErrValidation)
				}
				confidenceStr := ""
				if cmd.Flags().Changed("confidence") {
					confidenceStr = fmt.Sprintf("%.2f", confidence)
				}
				domainRelevanceStr := ""
				if cmd.Flags().Changed("domain-relevance") {
					domainRelevanceStr = fmt.Sprintf("%.2f", domainRelevance)
				}

				form := huh.NewForm(
					huh.NewGroup(
						huh.NewInput().
							Title("Voter").
							Value(&voter).
							Validate(func(s string) error {
								if strings.TrimSpace(s) == "" {
									return fmt.Errorf("voter is required")
								}
								return nil
							}),
						huh.NewInput().
							Title("Role").
							Value(&role),
						huh.NewSelect[string]().
							Title("Verdict").
							Options(
								huh.NewOption("approve", "approve"),
								huh.NewOption("approve-with-concerns", "approve-with-concerns"),
								huh.NewOption("reject", "reject"),
							).
							Value(&verdict),
						huh.NewInput().
							Title("Confidence (0.0-1.0)").
							Value(&confidenceStr).
							Validate(func(s string) error {
								if strings.TrimSpace(s) == "" {
									return fmt.Errorf("confidence is required")
								}
								var f float64
								if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
									return fmt.Errorf("confidence must be a number")
								}
								if f < 0.0 || f > 1.0 {
									return fmt.Errorf("confidence must be between 0.0 and 1.0")
								}
								return nil
							}),
						huh.NewInput().
							Title("Domain relevance (0.0-1.0)").
							Value(&domainRelevanceStr).
							Validate(func(s string) error {
								if strings.TrimSpace(s) == "" {
									return fmt.Errorf("domain relevance is required")
								}
								var f float64
								if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
									return fmt.Errorf("domain relevance must be a number")
								}
								if f < 0.0 || f > 1.0 {
									return fmt.Errorf("domain relevance must be between 0.0 and 1.0")
								}
								return nil
							}),
						huh.NewText().
							Title("Findings").
							Value(&findings),
						huh.NewInput().
							Title("Summary (one-line review summary)").
							Value(&summary),
					),
				)

				if err := form.Run(); err != nil {
					if errors.Is(err, huh.ErrUserAborted) {
						w.Info("Cancelled.")
						return nil
					}
					return cmdErr(fmt.Errorf("interactive form failed: %w", err), output.ErrGeneral)
				}

				// Parse form string values back to typed values.
				fmt.Sscanf(confidenceStr, "%f", &confidence)
				fmt.Sscanf(domainRelevanceStr, "%f", &domainRelevance)
			}

			// STDIN FEEDS EXACTLY ONE FLAG. Three flags accept "-" and they
			// all read the same pipe: the second reader finds it drained and
			// would store an empty value in silence. The refusal names every
			// flag that asked, because the caster has to know which pair
			// collided to fix the invocation.
			var stdinFlags []string
			if findings == "-" {
				stdinFlags = append(stdinFlags, "--findings")
			}
			if findingsJSONRaw == "-" {
				stdinFlags = append(stdinFlags, "--findings-json")
			}
			if summary == "-" {
				stdinFlags = append(stdinFlags, "--summary")
			}
			if len(stdinFlags) > 1 {
				return cmdErr(fmt.Errorf(
					"cannot read %s from stdin at once; stdin feeds one flag — pass the others inline, or use --summary-file",
					strings.Join(stdinFlags, " and ")), output.ErrValidation)
			}

			// Read findings from stdin if "-".
			if findings == "-" {
				s, err := readStdinFlag("--findings")
				if err != nil {
					return err
				}
				findings = s
			}

			// Read findings-json from stdin if "-".
			if findingsJSONRaw == "-" {
				s, err := readStdinFlag("--findings-json")
				if err != nil {
					return err
				}
				findingsJSONRaw = s
			}

			// THE SUMMARY HAS AN OFF-ARGV PATH (DKT-519). A seat's rationale
			// is routinely kilobytes of prose, and argv is the one channel
			// that runs it past a shell first: a summary carrying backticks
			// was once expanded by the shell and stored expanded, and a voter
			// casts once, so there was no amend path afterwards. `-` and
			// --summary-file both hand the bytes over untouched, mirroring
			// what --findings already offered.
			switch {
			case summary == "-":
				s, err := readStdinFlag("--summary")
				if err != nil {
					return err
				}
				summary = s
			case summaryFile != "":
				data, err := os.ReadFile(summaryFile)
				if err != nil {
					return cmdErr(fmt.Errorf("reading --summary-file: %w", err), output.ErrGeneral)
				}
				summary = strings.TrimRight(string(data), "\n")
			}

			// Parse and validate --findings-json.
			var findingsJSON *model.Findings
			if findingsJSONRaw != "" {
				var f model.Findings
				if err := json.Unmarshal([]byte(findingsJSONRaw), &f); err != nil {
					return cmdErr(fmt.Errorf("--findings-json is not valid JSON: %w", err), output.ErrValidation)
				}
				findingsJSON = &f
			}

			// Validate ranges.
			if confidence < 0.0 || confidence > 1.0 {
				return cmdErr(fmt.Errorf("--confidence must be in [0.0, 1.0]"), output.ErrValidation)
			}
			if domainRelevance < 0.0 || domainRelevance > 1.0 {
				return cmdErr(fmt.Errorf("--domain-relevance must be in [0.0, 1.0]"), output.ErrValidation)
			}

			// Validate verdict.
			if err := model.ValidateVerdict(model.Verdict(verdict)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}

			// Parse and validate --metadata: an opaque KV bag the casting seat
			// asserts about itself (which model and effort level cast this vote,
			// DKT-71), never a fact core verifies or reads a key out of.
			metadata, err := parseVoteMetadata(metadataRaw)
			if err != nil {
				return cmdErr(err, output.ErrValidation)
			}

			usageRaw, _ := cmd.Flags().GetString("usage")
			// A SEAT THAT REPORTS NOTHING IS TOLD SO (DKT-257). `--usage` stays
			// optional — a person casting a vote by hand has no token count to
			// give, and refusing them would make governance harder to use in
			// exactly the case it is least expensive — but silence must not be
			// free. In-wave panels are counted because their seats run as
			// steps; a conductor-side seat of identical shape recorded nothing,
			// and the only difference was where the ballot executed.
			//
			// stderr, not a refusal: the cast is valid and the vote must land.
			//
			// The note does NOT claim the spend is unrecoverable (DKT-519).
			// It once ended "nothing else will supply it later", which is
			// false for the fleet: a seat cannot measure its own tokens —
			// they live in the harness transcripts the conductor reads — so
			// `vote backfill-usage` (DKT-115) is the DESIGNED path, not a
			// workaround, and conductors run it minutes after a panel casts.
			// Naming it turns the note into the instruction it should have
			// been. `-q` silences it: a caster who has already chosen not to
			// report asked for no non-essential output, and the cast itself
			// still lands.
			if strings.TrimSpace(usageRaw) == "" && !w.QuietMode {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: this seat reported no --usage, so its spend is missing "+
						"from the run's totals rather than zero. A vote step is "+
						"never claimed, so the step ledger cannot hold it; record "+
						"it later with `docket vote backfill-usage %s` if a ledger "+
						"outside the seat holds it.\n",
					model.FormatProposalID(proposalID))
			}
			usage, err := parseVoteUsage(usageRaw)
			if err != nil {
				return cmdErr(err, output.ErrValidation)
			}

			vote := &model.Vote{
				ProposalID:      proposalID,
				VoterName:       voter,
				VoterRole:       role,
				Verdict:         model.Verdict(verdict),
				Confidence:      confidence,
				DomainRelevance: domainRelevance,
				Findings:        findings,
				FindingsJSON:    findingsJSON,
				Summary:         summary,
				Metadata:        metadata,
				Usage:           usage,
			}

			result, err := db.CastVote(conn, vote)
			if err != nil {
				if e := notFound(err, fmt.Sprintf("proposal %s", model.FormatProposalID(proposalID))); e != nil {
					return e
				}
				if errors.Is(err, db.ErrConflict) {
					return cmdErr(fmt.Errorf("conflict: voter %q has already voted on %s, or proposal is already finalized", voter, model.FormatProposalID(proposalID)), output.ErrConflict)
				}
				return cmdErr(fmt.Errorf("casting vote: %w", err), output.ErrGeneral)
			}

			// The cast that reaches quorum finalizes the proposal
			// (db.CastVote), and the vote STEP standing behind it — if this
			// is a run's vote-step ballot and not an ad-hoc one — must then
			// ROUTE, §8.1 phases 4 and 5. `next` cannot do it mid-wave (it
			// refuses while a dispatch is open), so the deciding cast is the
			// observing invocation (engine/drive.go): downstream staged rows
			// become claimable the moment this verb returns.
			if result.QuorumReached {
				e := engine.NewEngine()
				if err := e.DriveVoteProposal(conn, proposalID, model.NowMS()); err != nil {
					return cmdErr(fmt.Errorf(
						"the vote recorded and the proposal decided, but "+
							"routing the vote step failed: %w", err), output.ErrGeneral)
				}
			}

			// Build human-readable message.
			fmtID := model.FormatProposalID(proposalID)
			msg := fmt.Sprintf("Vote recorded for %s (%d/%d votes cast)", fmtID, result.VotesCast, result.VotesRequired)
			if result.QuorumReached && result.WeightedScore != nil {
				status := strings.ToUpper(string(result.ProposalStatus))
				msg = fmt.Sprintf("Vote recorded for %s (%d/%d votes cast) - %s (score: %.2f)", fmtID, result.VotesCast, result.VotesRequired, status, *result.WeightedScore)
			}

			data := voteCastResult{
				Vote:           result.Vote,
				ProposalStatus: string(result.ProposalStatus),
				VotesCast:      result.VotesCast,
				VotesRequired:  result.VotesRequired,
				QuorumReached:  result.QuorumReached,
				WeightedScore:  result.WeightedScore,
			}

			w.Success(data, msg)

			return nil
		},
	}

	cmd.Flags().String("voter", "", "Voter name (default: git user.name)")
	cmd.Flags().String("role", "", "Voter role")
	cmd.Flags().StringP("verdict", "v", "", "Vote: approve|approve-with-concerns|reject")
	cmd.Flags().Float64("confidence", 0, "Confidence 0.0-1.0")
	cmd.Flags().Float64("domain-relevance", 0, "Domain relevance 0.0-1.0")
	cmd.Flags().String("findings", "", "Review findings (use \"-\" for stdin)")
	cmd.Flags().String("findings-json", "", "Structured findings JSON (use \"-\" for stdin)")
	cmd.Flags().String("summary", "", "Review summary (use \"-\" for stdin)")
	cmd.Flags().String("summary-file", "",
		"Read the review summary from PATH (alternative to --summary; use when "+
			"stdin already feeds --findings)")
	cmd.Flags().String("metadata", "",
		"Opaque JSON object claiming what cast this vote — worked example in "+
			"skills/docket/SKILL.md; unverified, visible to anyone who can list "+
			"processes, and then stored and exported verbatim, so treat it as public")
	cmd.Flags().String("usage", "",
		`This seat's own spend report: {"unit": n, ...}, recorded per seat in `+
			"the vote-usage ledger and summed in the run report (a vote step "+
			"is never claimed, so the step ledger cannot hold it)")
	return cmd
}

var voteCastCmd = newVoteCastCmd()

func init() {
	voteCmd.AddCommand(voteCastCmd)
}

// readStdinFlag drains stdin for a flag whose value was given as "-", and is
// the ONE copy of that read: --findings, --findings-json and --summary all
// take "-" now, and three hand-inlined copies of the same limit-read is how
// one of them would end up with a different cap or a different trailing-
// newline rule than the others. The 1 MiB limit stops a wedged pipe from
// filling memory; the trailing newline goes because every shell here-doc and
// every `... > file` ends with one and no caster means it as content.
//
// A failure comes back already wrapped as a GENERAL_ERROR naming the flag, so
// a RunE call site is `return err`.
func readStdinFlag(flag string) (string, error) {
	const maxStdinSize = 1 << 20 // 1 MiB
	data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinSize))
	if err != nil {
		return "", cmdErr(fmt.Errorf("reading %s from stdin: %w", flag, err), output.ErrGeneral)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// parseVoteMetadata decodes a vote's opaque --metadata bag for THIS FLAG's
// sake: it turns caller text into a map and refuses the shapes a caster can
// still fix from the command line. The storage invariant — the size cap — is
// db.marshalVoteMetadata's, beside the column, so the import path is held to
// it too; the check here is the early, flag-named copy of that refusal.
//
// "Copy" is literal: the cap is applied to the ENCODED bag, the same quantity
// the column measures, rather than to the caller's raw text. The two differ in
// both directions — encoding/json escapes `<`, `>` and `&` into six bytes each
// and drops insignificant whitespace — so a raw-text cap would refuse bags the
// column accepts and accept bags the column refuses, under one constant that
// claims to be one limit.
//
// The "is this a JSON object" decode is engine.DecodeMetadataBag — the SAME
// rule `step complete --metadata` enforces — rather than a second copy of it;
// what stays HERE, thin around that call, is the one thing genuinely specific
// to this flag: the encoded-size cap against `db.VoteMetadataMaxBytes`, which
// the step-metadata path has no equivalent of (its own cap is checked on the
// raw input, before decoding, in engine.validateMetadataSize).
//
// KEYS AND VALUES ARE FREE TEXT and stay that way, exactly like --findings:
// core stores the bag whole and reads no key out of it, so there is no name
// to constrain and no value to interpret. Anything that renders a bag escapes
// it at its own boundary, as it already must for findings.
//
// An empty flag is the common case (a human `vote cast` asserting nothing
// about what produced it) and returns nil, nil — the vote's `metadata` column
// stays NULL, exactly as it did before this flag existed.
func parseVoteMetadata(raw string) (map[string]any, error) {
	bag, err := engine.DecodeMetadataBag(raw, "--metadata")
	if err != nil {
		return nil, err
	}
	if bag == nil {
		return nil, nil
	}

	encoded, err := json.Marshal(bag)
	if err != nil {
		return nil, fmt.Errorf("--metadata cannot be re-encoded: %w", err)
	}
	if len(encoded) > db.VoteMetadataMaxBytes {
		return nil, fmt.Errorf(
			"--metadata encodes to %d bytes, over the %d-byte cap; record the detail in --findings instead",
			len(encoded), db.VoteMetadataMaxBytes)
	}
	return bag, nil
}

// parseVoteUsage decodes a seat's --usage spend report through db.ParseUsage
// — the SAME B33/B35/B36 rules the step ledger's writer applies, one
// implementation for both flags — into the map shape model.Vote carries.
// db.CastVote re-checks the numeric rules beside the insert; this parse
// exists so a refusal reaches the operator as VALIDATION_ERROR naming the
// flag, before any transaction opens. Empty means the seat reports nothing,
// and nothing is written.
func parseVoteUsage(raw string) (map[string]float64, error) {
	rows, err := db.ParseUsage(raw)
	if err != nil {
		return nil, fmt.Errorf("--usage: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Unit] = r.Quantity
	}
	return out, nil
}
