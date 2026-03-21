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
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
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

var voteCastCmd = &cobra.Command{
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
		jsonMode, _ := cmd.Flags().GetBool("json")

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

		// Interactive form when not in JSON mode and required flags are missing.
		if !jsonMode && (verdict == "" || !cmd.Flags().Changed("confidence") || !cmd.Flags().Changed("domain-relevance")) {
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

		// Prevent both flags from reading stdin.
		if findings == "-" && findingsJSONRaw == "-" {
			return cmdErr(fmt.Errorf("cannot read both --findings and --findings-json from stdin"), output.ErrValidation)
		}

		// Read findings from stdin if "-".
		if findings == "-" {
			const maxStdinSize = 1 << 20 // 1 MiB
			data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinSize))
			if err != nil {
				return cmdErr(fmt.Errorf("reading findings from stdin: %w", err), output.ErrGeneral)
			}
			findings = strings.TrimRight(string(data), "\n")
		}

		// Read findings-json from stdin if "-".
		if findingsJSONRaw == "-" {
			const maxStdinSize = 1 << 20 // 1 MiB
			data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinSize))
			if err != nil {
				return cmdErr(fmt.Errorf("reading findings-json from stdin: %w", err), output.ErrGeneral)
			}
			findingsJSONRaw = strings.TrimRight(string(data), "\n")
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
		}

		result, err := db.CastVote(conn, vote)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("proposal %s not found", model.FormatProposalID(proposalID)), output.ErrNotFound)
			}
			if errors.Is(err, db.ErrConflict) {
				return cmdErr(fmt.Errorf("conflict: voter %q has already voted on %s, or proposal is already finalized", voter, model.FormatProposalID(proposalID)), output.ErrConflict)
			}
			return cmdErr(fmt.Errorf("casting vote: %w", err), output.ErrGeneral)
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

func init() {
	voteCastCmd.Flags().String("voter", "", "Voter name (default: git user.name)")
	voteCastCmd.Flags().String("role", "", "Voter role")
	voteCastCmd.Flags().StringP("verdict", "v", "", "Vote: approve|approve-with-concerns|reject")
	voteCastCmd.Flags().Float64("confidence", 0, "Confidence 0.0-1.0")
	voteCastCmd.Flags().Float64("domain-relevance", 0, "Domain relevance 0.0-1.0")
	voteCastCmd.Flags().String("findings", "", "Review findings (use \"-\" for stdin)")
	voteCastCmd.Flags().String("findings-json", "", "Structured findings JSON (use \"-\" for stdin)")
	voteCastCmd.Flags().String("summary", "", "One-line review summary")
	voteCmd.AddCommand(voteCastCmd)
}
