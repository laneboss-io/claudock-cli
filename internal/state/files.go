// Package state provides bounded, private file access for non-credential state.
package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var ErrUnsafe = errors.New("unsafe state file")

// Read refuses symlinks, devices, FIFOs, foreign-owned files, and oversized data.
func Read(path string, limit int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != uint32(os.Geteuid()) || info.Size() > limit {
		return nil, ErrUnsafe
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = ErrUnsafe
	}
	return data, err
}

// EnsureDirectory creates a state directory and refuses a symlink at its root.
func EnsureDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || st.Uid != uint32(os.Geteuid()) {
		return ErrUnsafe
	}
	return nil
}

// AtomicWrite fsyncs a private sibling before replacing the destination.
func AtomicWrite(path string, data []byte) error {
	base := filepath.Dir(path)
	if err := EnsureDirectory(base); err != nil {
		return err
	}
	f, err := os.CreateTemp(base, ".claudock-state-*.tmp")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err == nil {
		err = closed
	}
	if err == nil {
		err = os.Rename(temporary, path)
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(base)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// OpenLock refuses links and non-regular files before flock is used.
func OpenLock(path string) (*os.File, error) {
	if err := EnsureDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) {
			err = ErrUnsafe
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func TryLock(f *os.File, exclusive bool) error {
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	return syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
}

func Lock(f *os.File, exclusive bool, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := TryLock(f, exclusive)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func Unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
