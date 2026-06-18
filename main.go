// Command reposync keeps git repositories in sync across remote hosts.
package main

import "github.com/yasyf/reposync/internal/cli"

// version is injected at release time via -ldflags by goreleaser.
var version = "dev"

func main() {
	cli.Execute(version)
}
