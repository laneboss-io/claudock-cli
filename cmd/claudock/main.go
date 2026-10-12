// Command claudock runs Claude Code under one of several profiles and reads each profile's usage.
// It is the Go port of the Swift CLI in github.com/laneboss-io/claudock.
package main

import (
	"fmt"
	"os"
)

const version = "0.0.0-dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println("claudock " + version)
		return
	}
	fmt.Fprintln(os.Stderr, "claudock: this command is not ported to Go yet; use the Swift CLI from github.com/laneboss-io/claudock")
	os.Exit(70)
}
