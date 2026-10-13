//go:build !darwin || claudock_test

package policy

func readPreference(home string) (bool, error) { return fileReadPreference(home) }

func writePreference(home string, required bool) error {
	return fileWritePreference(home, required)
}
