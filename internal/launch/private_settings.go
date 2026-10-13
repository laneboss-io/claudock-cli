package launch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func privateEndpointSettings(data []byte) (string, func() error, error) {
	directory, err := os.MkdirTemp("", "claudock-settings-")
	if err != nil {
		return "", nil, fmt.Errorf("could not create private endpoint settings directory")
	}
	path := filepath.Join(directory, "settings.json")
	cleanup := func() error {
		fileErr := os.Remove(path)
		dirErr := os.Remove(directory)
		if (fileErr != nil && !errors.Is(fileErr, os.ErrNotExist)) ||
			(dirErr != nil && !errors.Is(dirErr, os.ErrNotExist)) {
			return fmt.Errorf("could not remove private endpoint settings")
		}
		return nil
	}
	fail := func() (string, func() error, error) {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return "", nil, cleanupErr
		}
		return "", nil, fmt.Errorf("could not write private endpoint settings")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail()
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return fail()
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fail()
	}
	return absolute, cleanup, nil
}
