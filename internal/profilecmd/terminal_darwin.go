//go:build darwin

package profilecmd

import (
	"errors"
	"sync"
	"syscall"
	"unsafe"
)

func hideTerminalInput(fd uintptr) (func(), bool, error) {
	var original syscall.Termios
	_, _, code := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&original)))
	if code == syscall.ENOTTY {
		return nil, false, nil
	}
	if code != 0 {
		return nil, false, errors.New("could not inspect terminal input")
	}
	hidden := original
	hidden.Lflag &^= syscall.ECHO | syscall.ECHONL
	_, _, code = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETAF, uintptr(unsafe.Pointer(&hidden)))
	if code != 0 {
		return nil, false, errors.New("could not hide terminal input; pipe the secret instead")
	}
	var once sync.Once
	return func() {
		once.Do(func() { syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSETAF, uintptr(unsafe.Pointer(&original))) })
	}, true, nil
}
