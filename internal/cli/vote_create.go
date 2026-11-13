package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var voteCreateCmd = &cobra.Command{
	Use:         "create",
	Short:       "Create a new consensus proposal",
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		env := verbEnv(cmd)

		// The project is refused before any flag check, the form or a stdin
		// read, so nobody types a proposal that is then thrown away.
		project, err := verb.VoteProject(cmd.Context(), env)
		recordRegisteredProject(cmd, env, project.Registered)
		if err != nil {
			return verbErr(err)
		}

		description, _ := cmd.Flags().GetString("description")
		rationale, _ := cmd.Flags().GetString("rationale")
		criticality, _ := cmd.Flags().GetString("criticality")
		voters, _ := cmd.Flags().GetInt("voters")
		threshold, _ := cmd.Flags().GetFloat64("threshold")
		createdBy, _ := cmd.Flags().GetString("created-by")
		domainTagsRaw, _ := cmd.Flags().GetString("domain-tags")
		filesChangedRaw, _ := cmd.Flags().GetString("files-changed")
		escalationReason, _ := cmd.Flags().GetString("escalation-reason")
		sealed, _ := cmd.Flags().GetBool("sealed")
		jsonMode, _ := jsonModeOf(cmd)

		// If JSON mode and no description, return validation error.
		if jsonMode && description == "" {
			return cmdErr(fmt.Errorf("--description is required in JSON mode"), output.ErrValidation)
		}

		// If JSON mode and no voters, return validation error.
		if jsonMode && !cmd.Flags().Changed("voters") {
			return cmdErr(fmt.Errorf("--voters is required"), output.ErrValidation)
		}

		// Determine if all required flags are present.
		allRequiredPresent := description != "" && cmd.Flags().Changed("voters")

		// Interactive form when not in JSON mode and required flags are missing.
		if !jsonMode && !allRequiredPresent {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				var missing []string
				if description == "" {
					missing = append(missing, "--description")
				}
				if !cmd.Flags().Changed("voters") {
					missing = append(missing, "--voters")
				}
				return cmdErr(fmt.Errorf("non-interactive environment detected; provide all required flags: %s", strings.Join(missing, ", ")), output.ErrValidation)
			}
			votersStr := ""
			if cmd.Flags().Changed("voters") {
				votersStr = fmt.Sprintf("%d", voters)
			}

			domainTagsInput := domainTagsRaw
			filesChangedInput := filesChangedRaw

			form := huh.NewForm(
				huh.NewGroup(
					huh.NewText().
						Title("Description").
						Value(&description).
						Validate(func(s string) error {
							if strings.TrimSpace(s) == "" {
								return fmt.Errorf("description is required")
							}
							return nil
						}),
					huh.NewText().
						Title("Rationale").
						Description("Why is this decision needed?").
						Value(&rationale),
					huh.NewSelect[string]().
						Title("Criticality").
						Options(
							huh.NewOption("low", "low"),
							huh.NewOption("medium", "medium"),
							huh.NewOption("high", "high"),
							huh.NewOption("critical", "critical"),
						).
						Value(&criticality),
					huh.NewInput().
						Title("Required voters").
						Value(&votersStr).
						Validate(func(s string) error {
							if strings.TrimSpace(s) == "" {
								return fmt.Errorf("voters is required")
							}
							var n int
							if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 1 {
								return fmt.Errorf("voters must be a positive integer")
							}
							return nil
						}),
					huh.NewInput().
						Title("Domain tags").
						Description("Comma-separated (e.g. cli,database,api)").
						Value(&domainTagsInput),
					huh.NewInput().
						Title("Files changed").
						Description("Comma-separated file paths").
						Value(&filesChangedInput),
				),
			)

			if err := form.Run(); err != nil {
				if errors.Is(err, huh.ErrUserAborted) {
					w.Info("Cancelled.")
					return nil
				}
				return cmdErr(fmt.Errorf("interactive form failed: %w", err), output.ErrGeneral)
			}

			// Parse voters from the form string.
			fmt.Sscanf(votersStr, "%d", &voters)

			// Capture form values back into raw flag variables.
			domainTagsRaw = domainTagsInput
			filesChangedRaw = filesChangedInput
		}

		// Read description from stdin if "-".
		if description == "-" {
			const maxStdinSize = 1 << 20 // 1 MiB
			data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinSize))
			if err != nil {
				return cmdErr(fmt.Errorf("reading description from stdin: %w", err), output.ErrGeneral)
			}
			description = strings.TrimRight(string(data), "\n")
		}

		// Read rationale from stdin if "-".
		if rationale == "-" {
			const maxStdinSize = 1 << 20 // 1 MiB
			data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinSize))
			if err != nil {
				return cmdErr(fmt.Errorf("reading rationale from stdin: %w", err), output.ErrGeneral)
			}
			rationale = strings.TrimRight(string(data), "\n")
		}

		// Parse comma-separated flags into slices.
		var domainTags []string
		if domainTagsRaw != "" {
			for _, tag := range strings.Split(domainTagsRaw, ",") {
				if t := strings.TrimSpace(tag); t != "" {
					domainTags = append(domainTags, t)
				}
			}
		}

		var filesChanged []string
		if filesChangedRaw != "" {
			for _, f := range strings.Split(filesChangedRaw, ",") {
				if p := strings.TrimSpace(f); p != "" {
					filesChanged = append(filesChanged, p)
				}
			}
		}

		in := verb.VoteCreateInput{
			Description:      description,
			Rationale:        rationale,
			Criticality:      criticality,
			Voters:           voters,
			Threshold:        threshold,
			CreatedBy:        createdBy,
			DomainTags:       domainTags,
			FilesChanged:     filesChanged,
			EscalationReason: escalationReason,
			Sealed:           sealed,
		}
		// The proposal's own fields are refused before an empty
		// --idempotency-key is.
		if err := in.Validate(); err != nil {
			return verbErr(err)
		}
		if in.IdempotencyKey, err = idempotencyKeyOf(cmd); err != nil {
			return err
		}

		res, err := verb.VoteCreate(cmd.Context(), env, in)
		recordRegisteredProject(cmd, env, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		created := res.Proposal
		w.Success(created, fmt.Sprintf("Created %s: %s", model.FormatProposalID(created.ID), created.Description))

		return nil
	},
}

func init() {
	voteCreateCmd.Flags().StringP("description", "d", "", "Proposal description (use \"-\" for stdin)")
	voteCreateCmd.Flags().StringP("rationale", "r", "", "Rationale for the proposal (use \"-\" for stdin)")
	voteCreateCmd.Flags().StringP("criticality", "c", "medium", "Criticality level: low|medium|high|critical")
	voteCreateCmd.Flags().IntP("voters", "n", 0, "Required number of voters")
	voteCreateCmd.Flags().Float64("threshold", 0.67, "Approval threshold 0.0-1.0")
	voteCreateCmd.Flags().String("created-by", "", "Creator identity (default: git user.name)")
	voteCreateCmd.Flags().String("domain-tags", "", "Comma-separated domain tags (e.g. cli,database,api)")
	voteCreateCmd.Flags().String("files-changed", "", "Comma-separated file paths affected by this proposal")
	voteCreateCmd.Flags().String("escalation-reason", "", "Reason for escalation (if applicable)")
	voteCreateCmd.Flags().Bool("sealed", false,
		"Withhold each cast's verdict, weights, findings and summary from "+
			"vote show/result until the tally closes the proposal; the same rule "+
			"vote.rule.<name>.sealed applies to a workflow vote step")
	addIdempotencyKeyFlag(voteCreateCmd)
	voteCmd.AddCommand(voteCreateCmd)
}
