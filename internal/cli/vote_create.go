package cli

import (
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

var voteCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new consensus proposal",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		description, _ := cmd.Flags().GetString("description")
		criticality, _ := cmd.Flags().GetString("criticality")
		voters, _ := cmd.Flags().GetInt("voters")
		threshold, _ := cmd.Flags().GetFloat64("threshold")
		createdBy, _ := cmd.Flags().GetString("created-by")
		jsonMode, _ := cmd.Flags().GetBool("json")

		// Default created-by to git user.name.
		if createdBy == "" {
			createdBy = config.DefaultAuthor()
		}

		// If JSON mode and no description, return validation error.
		if jsonMode && description == "" {
			return cmdErr(fmt.Errorf("--description is required in JSON mode"), output.ErrValidation)
		}

		// If JSON mode and no voters, return validation error.
		if jsonMode && !cmd.Flags().Changed("voters") {
			return cmdErr(fmt.Errorf("--voters is required"), output.ErrValidation)
		}

		// If no description and not JSON mode, launch interactive form.
		if !jsonMode && description == "" {
			votersStr := ""
			if cmd.Flags().Changed("voters") {
				votersStr = fmt.Sprintf("%d", voters)
			}

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

		// Validate voters.
		if voters < 1 {
			return cmdErr(fmt.Errorf("--voters must be >= 1"), output.ErrValidation)
		}

		// Validate threshold.
		if threshold <= 0.0 || threshold > 1.0 {
			return cmdErr(fmt.Errorf("--threshold must be in (0.0, 1.0]"), output.ErrValidation)
		}

		// Validate criticality.
		if err := model.ValidateCriticality(model.Criticality(criticality)); err != nil {
			return cmdErr(err, output.ErrValidation)
		}

		proposal := model.Proposal{
			Description:    description,
			Criticality:    model.Criticality(criticality),
			Status:         model.ProposalStatusOpen,
			RequiredVoters: voters,
			Threshold:      threshold,
			CreatedBy:      createdBy,
		}

		id, err := db.CreateProposal(conn, &proposal)
		if err != nil {
			return cmdErr(fmt.Errorf("creating proposal: %w", err), output.ErrGeneral)
		}

		// Refetch to get full object with timestamps.
		created, err := db.GetProposal(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching created proposal: %w", err), output.ErrGeneral)
		}

		w.Success(created, fmt.Sprintf("Created %s: %s", model.FormatProposalID(id), created.Description))

		return nil
	},
}

func init() {
	voteCreateCmd.Flags().StringP("description", "d", "", "Proposal description (use \"-\" for stdin)")
	voteCreateCmd.Flags().StringP("criticality", "c", "medium", "Criticality level: low|medium|high|critical")
	voteCreateCmd.Flags().IntP("voters", "n", 0, "Required number of voters")
	voteCreateCmd.Flags().Float64("threshold", 0.67, "Approval threshold 0.0-1.0")
	voteCreateCmd.Flags().String("created-by", "", "Creator identity (default: git user.name)")
	voteCmd.AddCommand(voteCreateCmd)
}
