// Command reposync keeps git repositories in sync across remote hosts.
package main

import (
	"fmt"
	"os"

	"github.com/yasyf/daemonkit/trust"

	"github.com/yasyf/reposync/internal/cli"
)

var version = "dev"

func main() {
	if handled, err := trust.RunVerifierChild(os.Args[1:], os.Stdout); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	cli.Execute(version)
}
