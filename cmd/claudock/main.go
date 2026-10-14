// Command claudock runs Claude Code under one of several profiles and reads each profile's usage.
// It is the Go port of the Swift CLI in github.com/laneboss-io/claudock.
package main

import (
	"fmt"
	"os"

	"github.com/laneboss-io/claudock-cli/internal/launch"
	"github.com/laneboss-io/claudock-cli/internal/profilecmd"
	"github.com/laneboss-io/claudock-cli/internal/usage"
)

func main() {
	// usage, available, require-token and version
	if handled, code := usage.Run(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	// run and shell
	if handled, status := launch.Command(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(status)
	}
	if len(os.Args) == 1 || (len(os.Args) == 2 && (os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h")) {
		fmt.Println("Claudock " + usage.Version)
		fmt.Println("  claudock --version")
		fmt.Println()
		fmt.Print(profilecmd.Help)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "profile" {
		os.Exit(profilecmd.Execute(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	fmt.Fprintln(os.Stderr, "claudock: unknown command; `claudock help` lists the commands")
	os.Exit(2)
}
