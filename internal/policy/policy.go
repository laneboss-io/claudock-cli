// Package policy shares the inference-token requirement with the Swift app.
package policy

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/state"
)

const Domain = "io.github.claudeusage.ClaudeUsage"
const Key = "requireInferenceToken"

var ErrRead = errors.New("Claudock could not read the inference token requirement. Try again.")
var ErrSave = errors.New("Claudock could not save the inference token requirement. Try again.")

// Required reads the shared preference. Callers must propagate an unreadable
// setting rather than treating it as permission to launch without a token.
func Required() (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, ErrRead
	}
	return readPreference(home)
}

func SetRequired(required bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return ErrSave
	}
	return writePreference(home, required)
}

type plistNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Inner   string     `xml:",innerxml"`
}

type pair struct {
	key   string
	value plistNode
}

func preferenceFile(home string) string {
	return filepath.Join(home, "Library", "Preferences", Domain+".plist")
}

func parsePlist(data []byte) ([]pair, error) {
	var root struct {
		XMLName xml.Name   `xml:"plist"`
		Dict    *plistNode `xml:"dict"`
	}
	if err := xml.Unmarshal(data, &root); err != nil || root.Dict == nil {
		return nil, ErrRead
	}
	d := xml.NewDecoder(strings.NewReader(root.Dict.Inner))
	var pairs []pair
	seen := make(map[string]bool)
	for {
		token, err := d.Token()
		if err == io.EOF {
			return pairs, nil
		}
		if err != nil {
			return nil, ErrRead
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			if chars, ok := token.(xml.CharData); ok && strings.TrimSpace(string(chars)) != "" {
				return nil, ErrRead
			}
			continue
		}
		if start.Name.Local != "key" {
			return nil, ErrRead
		}
		var key string
		if err := d.DecodeElement(&key, &start); err != nil || seen[key] {
			return nil, ErrRead
		}
		seen[key] = true
		for {
			token, err = d.Token()
			if err != nil {
				return nil, ErrRead
			}
			if chars, ok := token.(xml.CharData); ok {
				if strings.TrimSpace(string(chars)) != "" {
					return nil, ErrRead
				}
				continue
			}
			if _, ok := token.(xml.Comment); ok {
				continue
			}
			break
		}
		valueStart, ok := token.(xml.StartElement)
		if !ok {
			return nil, ErrRead
		}
		var value plistNode
		if err := d.DecodeElement(&value, &valueStart); err != nil {
			return nil, ErrRead
		}
		pairs = append(pairs, pair{key: key, value: value})
	}
}

func requiredIn(pairs []pair) (bool, error) {
	for _, p := range pairs {
		if p.key != Key {
			continue
		}
		switch p.value.XMLName.Local {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "integer":
			value, err := strconv.ParseInt(strings.TrimSpace(p.value.Inner), 10, 64)
			if err == nil {
				return value != 0, nil
			}
		}
		return false, ErrRead
	}
	return false, nil
}

func fileReadPreference(home string) (bool, error) {
	data, err := state.Read(preferenceFile(home), 1_048_576)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, ErrRead
	}
	pairs, err := parsePlist(data)
	if err != nil {
		return false, err
	}
	return requiredIn(pairs)
}

func fileWritePreference(home string, required bool) error {
	base := filepath.Dir(preferenceFile(home))
	lock, err := state.OpenLock(filepath.Join(base, ".claudock-require-token-lock"))
	if err != nil {
		return ErrSave
	}
	defer state.Unlock(lock)
	if state.Lock(lock, true, 5*time.Second) != nil {
		return ErrSave
	}
	var pairs []pair
	data, err := state.Read(preferenceFile(home), 1_048_576)
	if err == nil {
		pairs, err = parsePlist(data)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return ErrSave
	}
	if _, err := requiredIn(pairs); err != nil {
		return ErrSave
	}
	var result []pair
	for _, p := range pairs {
		if p.key != Key {
			result = append(result, p)
		}
	}
	if required {
		result = append(result, pair{key: Key, value: plistNode{XMLName: xml.Name{Local: "true"}}})
	}
	var buffer bytes.Buffer
	buffer.WriteString(xml.Header)
	buffer.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n")
	e := xml.NewEncoder(&buffer)
	for _, p := range result {
		if e.EncodeElement(p.key, xml.StartElement{Name: xml.Name{Local: "key"}}) != nil || e.Encode(p.value) != nil {
			return ErrSave
		}
	}
	if e.Flush() != nil {
		return ErrSave
	}
	buffer.WriteString("\n</dict></plist>\n")
	if buffer.Len() > 1_048_576 || state.AtomicWrite(preferenceFile(home), buffer.Bytes()) != nil {
		return ErrSave
	}
	readback, err := fileReadPreference(home)
	if err != nil || readback != required {
		return ErrSave
	}
	return nil
}
