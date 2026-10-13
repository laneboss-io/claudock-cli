package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ReplaceCredentials updates an existing OAuth record using a compare-and-swap on its complete JSON.
// It retains the original backend, preserves unmentioned JSON fields, and cooperates with Claude Code's
// .storage-write.lock directory lease. The original argument comes from Secret(profile, "credentials").
func ReplaceCredentials(p Profile, original, replacement string) error {
	previous, err := oauthObject([]byte(original))
	if err != nil {
		return itemError(p, OAuthItem, "original OAuth record is invalid")
	}
	next, err := oauthObject([]byte(replacement))
	if err != nil {
		return itemError(p, OAuthItem, "replacement OAuth record is invalid")
	}
	var previousOAuth, nextOAuth map[string]json.RawMessage
	if json.Unmarshal(previous["claudeAiOauth"], &previousOAuth) != nil || json.Unmarshal(next["claudeAiOauth"], &nextOAuth) != nil {
		return itemError(p, OAuthItem, "OAuth record is invalid")
	}
	for key, value := range previousOAuth {
		if _, present := nextOAuth[key]; !present {
			nextOAuth[key] = value
		}
	}
	for key, value := range previous {
		if key != "claudeAiOauth" {
			if _, present := next[key]; !present {
				next[key] = value
			}
		}
	}
	next["claudeAiOauth"], err = json.Marshal(nextOAuth)
	if err != nil {
		return itemError(p, OAuthItem, "OAuth record could not be encoded")
	}
	data, err := json.Marshal(next)
	if err != nil || len(data) > maximumCredentialBytes {
		return itemError(p, OAuthItem, "OAuth record is too large")
	}
	expected, err := json.Marshal(previous)
	if err != nil {
		return itemError(p, OAuthItem, "OAuth record is invalid")
	}
	return withSnapshot(false, func(s *snapshot) error {
		if _, _, err := currentProfile(s, p); err != nil {
			return err
		}
		service, err := Service(p, OAuthItem)
		if err != nil {
			return err
		}
		b, err := backend()
		if err != nil {
			return itemError(p, OAuthItem, err.Error())
		}
		return storageLease(p.ConfigDirectory, func(assertOwned func() error) error {
			if err := assertOwned(); err != nil {
				return err
			}
			current, err := b.read(service)
			if errors.Is(err, ErrNoSecret) {
				path := filepath.Join(p.ConfigDirectory, ".credentials.json")
				if err := credentialParent(p.ConfigDirectory); err != nil {
					return itemError(p, OAuthItem, "credential parent is unsafe or missing")
				}
				info, err := os.Lstat(path)
				if err != nil {
					return itemError(p, OAuthItem, "credential file changed or was removed")
				}
				current, err = readFile(path, maximumCredentialBytes, true)
				if err != nil || !sameOAuthJSON(current, expected) {
					return itemError(p, OAuthItem, "credential file changed or was removed")
				}
				if err := assertOwned(); err != nil {
					return err
				}
				if sameOAuthJSON(current, data) {
					return nil
				}
				linked, err := os.Lstat(path)
				if err != nil || !os.SameFile(info, linked) || !unchanged(path, current, maximumCredentialBytes) {
					return itemError(p, OAuthItem, "credential file changed or was removed")
				}
				if err := atomicWrite(path, data); err != nil {
					return itemError(p, OAuthItem, err.Error())
				}
				return assertOwned()
			}
			if err != nil {
				return itemError(p, OAuthItem, err.Error())
			}
			if !sameOAuthJSON(current, expected) {
				return itemError(p, OAuthItem, "OAuth credential changed while refreshing")
			}
			if sameOAuthJSON(current, data) {
				return assertOwned()
			}
			if file, ok := b.(fileCredentials); ok {
				return file.locked(true, func(state *credentialFile) error {
					value, present := state.Items[service]
					if !present || !sameOAuthJSON([]byte(value), expected) {
						return itemError(p, OAuthItem, "OAuth credential changed while refreshing")
					}
					if err := assertOwned(); err != nil {
						return err
					}
					state.Items[service] = string(data)
					return nil
				})
			}
			if err := assertOwned(); err != nil {
				return err
			}
			if err := b.write(service, data); err != nil {
				return itemError(p, OAuthItem, err.Error())
			}
			return assertOwned()
		})
	})
}

func oauthObject(data []byte) (map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if decodeCredentialJSON(data, &root) != nil || root == nil {
		return nil, errors.New("invalid OAuth record")
	}
	var oauth map[string]json.RawMessage
	if decodeJSON(root["claudeAiOauth"], &oauth) != nil || oauth == nil {
		return nil, errors.New("invalid OAuth record")
	}
	var access string
	if json.Unmarshal(oauth["accessToken"], &access) != nil || access == "" {
		return nil, errors.New("invalid OAuth record")
	}
	return root, nil
}

func sameOAuthJSON(first, second []byte) bool {
	a, err := oauthObject(first)
	if err != nil {
		return false
	}
	b, err := oauthObject(second)
	if err != nil {
		return false
	}
	canonical := func(value map[string]json.RawMessage) ([]byte, error) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var object any
		if err := decoder.Decode(&object); err != nil {
			return nil, err
		}
		return json.Marshal(object)
	}
	x, err := canonical(a)
	if err != nil {
		return false
	}
	y, err := canonical(b)
	return err == nil && bytes.Equal(x, y)
}

func storageLease(directory string, operation func(func() error) error) error {
	if err := credentialParent(directory); err != nil {
		return errors.New("OAuth credential directory is unsafe or missing")
	}
	parent, err := os.Stat(directory)
	if err != nil {
		return errors.New("OAuth credential directory is unavailable")
	}
	path := filepath.Join(directory, ".storage-write.lock")
	deadline := time.Now().Add(3 * time.Second)
	for {
		err = os.Mkdir(path, 0700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return errors.New("OAuth storage lease could not be acquired")
		}
		entry, err := os.Lstat(path)
		if err != nil || !entry.IsDir() {
			return errors.New("OAuth storage lease is unsafe")
		}
		if time.Now().After(deadline) {
			return ErrBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("OAuth storage lease could not be inspected; its directory was retained")
	}
	handle := os.NewFile(uintptr(fd), "OAuth storage lease")
	defer handle.Close()
	owned, err := handle.Stat()
	if err != nil {
		return errors.New("OAuth storage lease could not be inspected; its directory was retained")
	}
	var mu sync.Mutex
	var heartbeatErr error
	assertOwned := func() error {
		mu.Lock()
		defer mu.Unlock()
		if heartbeatErr != nil {
			return heartbeatErr
		}
		currentParent, parentErr := os.Stat(directory)
		current, entryErr := os.Lstat(path)
		if parentErr != nil || entryErr != nil || !os.SameFile(parent, currentParent) || !os.SameFile(owned, current) || time.Since(current.ModTime()) >= 15*time.Second {
			return errors.New("OAuth storage lease changed while refreshing")
		}
		return nil
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				now := syscall.NsecToTimeval(time.Now().UnixNano())
				if err := syscall.Futimes(fd, []syscall.Timeval{now, now}); err != nil {
					heartbeatErr = errors.New("OAuth storage lease heartbeat failed")
				}
				mu.Unlock()
			}
		}
	}()
	err = operation(assertOwned)
	close(done)
	<-stopped
	ownershipErr := assertOwned()
	if ownershipErr == nil {
		if removeErr := os.Remove(path); removeErr != nil && err == nil {
			err = errors.New("OAuth storage lease could not be released")
		}
	} else if err == nil {
		err = ownershipErr
	}
	return err
}
