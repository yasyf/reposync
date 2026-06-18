// Command reposync keeps git repositories in sync across remote hosts.
package main

import "github.com/yasyf/reposync/internal/cli"

var version = "dev"

func main() {
	cli.Execute(version)
}
