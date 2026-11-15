package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/dustin/go-humanize"
	"github.com/geektr-cloud/openmgcl/internal/tacos"
)

func writeStateSummaries(out io.Writer, summaries []tacos.StateSummary, withSize bool) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if withSize {
		fmt.Fprintln(w, "STATE\tVERSIONS\tPENDING\tSIZE")
	} else {
		fmt.Fprintln(w, "STATE\tVERSIONS\tPENDING")
	}
	for _, summary := range summaries {
		state := summary.Organization + "/" + summary.Name
		if withSize {
			fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", state, summary.Versions, summary.Pending, humanize.IBytes(summary.StoredBytes))
			continue
		}
		fmt.Fprintf(w, "%s\t%d\t%d\n", state, summary.Versions, summary.Pending)
	}
	return w.Flush()
}

func writeStateVersions(out io.Writer, versions []tacos.StateVersionInfo) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSERIAL\tSTATUS\tCURRENT\tCREATED")
	for _, version := range versions {
		fmt.Fprintf(w, "%s\t%d\t%s\t%t\t%s\n", version.ID, version.Serial, version.Status, version.Current, version.CreatedAt)
	}
	return w.Flush()
}
