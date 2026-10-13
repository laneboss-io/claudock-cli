package launch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unicode/utf8"
)

// Read without allowing a FIFO or device to block a launch. Existing symlinks
// are allowed for shared profile settings, but shell ownership uses noFollow.
func readRegular(path string, limit int64, noFollow bool) ([]byte, error) {
	flags := syscall.O_RDONLY | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	if noFollow {
		flags |= syscall.O_NOFOLLOW
	}
	fd, err := syscall.Open(path, flags, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("cannot read bounded file")
	}
	after, err := f.Stat()
	current, pathErr := os.Stat(path)
	if err != nil || pathErr != nil || !os.SameFile(before, after) || !os.SameFile(after, current) ||
		before.Size() != after.Size() || after.Size() != int64(len(data)) ||
		!before.ModTime().Equal(after.ModTime()) || !after.ModTime().Equal(current.ModTime()) {
		return nil, errors.New("file changed while reading")
	}
	return data, nil
}

// Settings must have one interpretation. In particular, duplicate names are
// rejected before encoding/json can silently keep the last occurrence.
func jsonObject(data []byte) (map[string]any, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return nil, errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		if depth > 256 {
			return nil, errors.New("JSON too deep")
		}
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				object := make(map[string]any)
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return nil, err
					}
					name, ok := key.(string)
					if !ok {
						return nil, errors.New("invalid JSON name")
					}
					if _, exists := object[name]; exists {
						return nil, errors.New("duplicate JSON name")
					}
					object[name], err = value(depth + 1)
					if err != nil {
						return nil, err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim('}') {
					return nil, errors.New("invalid JSON object")
				}
				return object, nil
			case '[':
				var array []any
				for d.More() {
					item, err := value(depth + 1)
					if err != nil {
						return nil, err
					}
					array = append(array, item)
				}
				end, err := d.Token()
				if err != nil || end != json.Delim(']') {
					return nil, errors.New("invalid JSON array")
				}
				return array, nil
			default:
				return nil, errors.New("invalid JSON delimiter")
			}
		}
		return token, nil
	}
	result, err := value(0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("extra JSON input")
	}
	object, ok := result.(map[string]any)
	if !ok {
		return nil, errors.New("JSON must be an object")
	}
	return object, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".claudock-*.tmp")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	defer f.Close()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
