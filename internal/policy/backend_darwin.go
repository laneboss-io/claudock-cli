//go:build darwin && !claudock_test

package policy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// defaults talks to CFPreferences, keeping the resident app's view coherent
// even when its preferences domain is stored as a binary plist.
func defaults(arguments ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/defaults", arguments...)
	var out, diagnostics bytes.Buffer
	command.Stdout, command.Stderr = &out, &diagnostics
	err := command.Run()
	return out.Bytes(), diagnostics.Bytes(), err
}

func readPreference(home string) (bool, error) {
	// Tests using the required file credential backend stay entirely in HOME,
	// including when an ordinary Darwin binary is used by an integration test.
	if strings.HasPrefix(os.Getenv("CLAUDOCK_CREDENTIAL_STORE"), "file:") {
		return fileReadPreference(home)
	}
	out, diagnostic, err := defaults("read", Domain, Key)
	if err != nil {
		if strings.Contains(string(diagnostic), "does not exist") {
			return false, nil
		}
		return false, ErrRead
	}
	switch strings.TrimSpace(string(out)) {
	case "1", "true", "YES":
		return true, nil
	case "0", "false", "NO":
		return false, nil
	default:
		return false, ErrRead
	}
}

func writePreference(home string, required bool) error {
	if strings.HasPrefix(os.Getenv("CLAUDOCK_CREDENTIAL_STORE"), "file:") {
		return fileWritePreference(home, required)
	}
	var err error
	if required {
		_, _, err = defaults("write", Domain, Key, "-bool", "true")
	} else {
		_, diagnostic, readErr := defaults("read", Domain, Key)
		if readErr != nil && !strings.Contains(string(diagnostic), "does not exist") {
			return ErrSave
		}
		if readErr == nil {
			_, _, err = defaults("delete", Domain, Key)
		}
	}
	if err != nil {
		return ErrSave
	}
	readback, err := readPreference(home)
	if err != nil || readback != required {
		return ErrSave
	}
	return nil
}
