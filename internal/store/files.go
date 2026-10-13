package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const maximumRegistryBytes = 2 * 1024 * 1024

var errPublicationUncertain = errors.New("store file was published, but directory sync failed; its data was preserved")

func validPath(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	for _, r := range path {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("could not prepare store parent")
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("could not create store directory")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("store directory must be a real owned directory")
	}
	defer syscall.Close(fd)
	var info syscall.Stat_t
	if syscall.Fstat(fd, &info) != nil || info.Uid != uint32(os.Geteuid()) {
		return errors.New("store directory must be owned by this user")
	}
	if syscall.Fchmod(fd, 0700) != nil {
		return errors.New("could not protect store directory")
	}
	return nil
}

// readFile refuses links, devices, FIFOs, foreign owners and hard-linked managed files.
func readFile(path string, limit int64, secret bool) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("store file is unavailable or unsafe")
	}
	f := os.NewFile(uintptr(fd), "store")
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, errors.New("could not inspect store file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || info.Size() < 0 || info.Size() > limit || (secret && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("store file is unsafe")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) != info.Size() || int64(len(data)) > limit {
		return nil, errors.New("could not read bounded store file")
	}
	after, err := f.Stat()
	linked, linkErr := os.Lstat(path)
	if err != nil || linkErr != nil || !os.SameFile(info, linked) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("store file changed while reading")
	}
	return data, nil
}

func readOptional(path string, limit int64, secret bool) ([]byte, error) {
	data, err := readFile(path, limit, secret)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func atomicWrite(path string, data []byte) error {
	if _, err := os.Lstat(path); err == nil {
		if _, err = readFile(path, 32*1024*1024, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not inspect destination")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".claudock-*.tmp")
	if err != nil {
		return errors.New("could not create store temporary file")
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return errors.New("could not protect store file")
	}
	if _, err = f.Write(data); err != nil {
		return errors.New("could not write store file")
	}
	if err = f.Sync(); err != nil {
		return errors.New("could not sync store file")
	}
	if err = f.Close(); err != nil {
		return errors.New("could not close store file")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("could not open store directory for sync")
	}
	defer dir.Close()
	if err = os.Rename(name, path); err != nil {
		return errors.New("could not publish store file")
	}
	if err = dir.Sync(); err != nil {
		return errPublicationUncertain
	}
	return nil
}

func withLock(path string, exclusive bool, operation func() error) error {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return errors.New("store lock is unavailable or unsafe")
	}
	defer syscall.Close(fd)
	var info syscall.Stat_t
	if syscall.Fstat(fd, &info) != nil || info.Mode&syscall.S_IFMT != syscall.S_IFREG || info.Nlink != 1 || info.Uid != uint32(os.Geteuid()) || syscall.Fchmod(fd, 0600) != nil {
		return errors.New("store lock is unsafe")
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Flock(fd, mode|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if (!errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR)) || time.Now().After(deadline) {
			return ErrBusy
		}
		time.Sleep(15 * time.Millisecond)
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return operation()
}

func unchanged(path string, before []byte, limit int64) bool {
	after, err := readOptional(path, limit, false)
	return err == nil && bytes.Equal(before, after)
}
