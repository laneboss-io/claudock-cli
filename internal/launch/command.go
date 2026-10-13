// Package launch implements profile launches and the optional zsh adapter.
// Credentials are obtained exclusively through the shared store interface.
package launch

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

type commandError struct {
	message string
	status  int
}

func (e *commandError) Error() string { return e.message }

func invalid(message string) error { return &commandError{message, 2} }

// Command handles this lane's commands. Other commands belong to other lanes.
// A successful run replaces this process, preserving its terminal and signals.
func Command(args []string, out, errOut io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	var err error
	switch {
	case args[0] == "run":
		err = run(args[1:], errOut)
	case args[0] == "shell":
		err = shellCommand(args[1:], out)
	case len(args) >= 2 && args[0] == "profile" && args[1] == "swap":
		err = swap(args[2:], out, errOut)
	default:
		return false, 0
	}
	if err == nil {
		return true, 0
	}
	var exited *childExit
	if errors.As(err, &exited) {
		return true, exited.status
	}
	fmt.Fprintln(errOut, "claudock: "+err.Error())
	var failure *commandError
	if errors.As(err, &failure) {
		return true, failure.status
	}
	return true, 1
}

func validName(name string) bool {
	if name == "" || strings.HasPrefix(strings.ToLower(name), "sk-") {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return false
		}
	}
	return name[0] != '-'
}

func validPath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.ContainsFunc(path, unicode.IsControl)
}

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
