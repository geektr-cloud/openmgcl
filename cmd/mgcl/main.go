// Command mgcl is the installable entry point: go install
// github.com/geektr-cloud/openmgcl/cmd/mgcl@<version> produces a binary named
// mgcl. It shares cmd.Main with the repository root main package.
package main

import "github.com/geektr-cloud/openmgcl/cmd"

func main() {
	cmd.Main()
}
