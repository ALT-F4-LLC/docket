package output

import (
	"fmt"
	"io"
)

// writeHumanSuccess writes a human-readable success message to w.
func writeHumanSuccess(w io.Writer, message string) {
	if message != "" {
		fmt.Fprintln(w, message)
	}
}

// writeHumanError writes a human-readable error message to w.
func writeHumanError(w io.Writer, err error) {
	fmt.Fprintf(w, "Error: %s\n", err)
}
