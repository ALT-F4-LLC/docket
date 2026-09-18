package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var pinCmd = &cobra.Command{
	Use:   "pin",
	Short: "Read what a run pinned",
}

var pinShowCmd = &cobra.Command{
	Use:   "show RUN-N PATH",
	Short: "Print the bytes a run pinned at a config-relative path",
	Long: `Print the content of the blob RUN-N pinned at PATH when it activated.

A rendered packet lists its pinned files as a ref and a sha256, so a contract
asking a worker to test something against ` + "`policy.toml`" + ` named a document
the worker could not open: ` + "`docket policy resolve`" + ` answers with the
routing derived from that file, not with its text.

PATH resolves exactly as a packet entry does — the config-relative ref at each
instance-config root, then the legacy absolute form a pre-v12 run recorded — so
these are the bytes a packet carrying the file would have carried.

A path the run never pinned is refused (VALIDATION_ERROR) rather than read off
disk: the pin set froze at activation, and a file's presence cannot admit it.
A file edited since activation refuses too, naming both hashes, because the pin
is the run's agreement about bytes and printing the edit would substitute
content the run never agreed to. ` + "`docket run verify-pins`" + ` reports the
same drift across the whole pin set.

READ-ONLY: no lease, no lock, no event.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPinShow(cmd, args[0], args[1], getWriter(cmd))
	},
}

func runPinShow(cmd *cobra.Command, ref, path string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	body, err := engine.PinContent(conn, runID, path)
	if err != nil {
		return runErr(err)
	}

	// THE BYTES GO OUT EXACTLY AS PINNED on the human path — written to Stdout
	// directly rather than through Success, which decorates a message and
	// appends a newline. The verb's whole promise is that what it prints hashes
	// to the pin, so `docket pin show ... | shasum` has to agree with
	// `docket run verify-pins`; a trailing byte of framing breaks that.
	if !w.JSONMode {
		fmt.Fprint(w.Stdout, body)
		return nil
	}
	w.Success(pinShowPayload{Run: model.FormatRunID(runID), Path: path, Body: body}, "")
	return nil
}

// pinShowPayload is the `--json` shape: the bytes, with the run and path they
// came from so a reader can tell which pin answered.
type pinShowPayload struct {
	Run  string `json:"run"`
	Path string `json:"path"`
	Body string `json:"body"`
}

func init() {
	pinCmd.AddCommand(pinShowCmd)
	rootCmd.AddCommand(pinCmd)
}
