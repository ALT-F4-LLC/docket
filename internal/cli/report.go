package cli

import (
	"github.com/spf13/cobra"
)

// `docket report` — cross-run rollups over the store (DKT-2453).
//
// `run report` answers for ONE run. The verbs under this group answer across
// runs, for the questions a retro asks of the store as a whole. Every one of
// them is READ-ONLY and OPERATOR-FACING: nothing here is consulted by `next`
// or by any routing decision, and nothing here is handed to a seat.
var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "Read-only rollups across runs, for retro",
	Long: `Roll facts up across runs, for an operator reading the store at retro.

READ-ONLY. Every verb here computes its rows at read and WRITES NOTHING. None
of them is consulted by ` + "`next`" + ` or by any routing decision, and none is handed
to a seat: a track record fed back into a panel becomes an incentive to agree
with it, which is the failure the record exists to catch.`,
}

func init() {
	rootCmd.AddCommand(reportCmd)
}
