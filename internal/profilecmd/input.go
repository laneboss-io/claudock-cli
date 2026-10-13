package profilecmd

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"unicode/utf8"
)

func readSecretInput(in io.Reader, stderr io.Writer) (string, error) {
	terminal := false
	var restore func()
	if file, ok := in.(*os.File); ok {
		var err error
		restore, terminal, err = hideTerminalInput(file.Fd())
		if err != nil {
			return "", err
		}
	}
	if terminal {
		defer restore()
		if _, err := io.WriteString(stderr, "Key or token: "); err != nil {
			return "", errors.New("could not write secret prompt")
		}
		signals := make(chan os.Signal, 1)
		done := make(chan struct{})
		var once sync.Once
		signal.Notify(signals, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGPIPE, syscall.SIGALRM)
		defer func() { signal.Stop(signals); once.Do(func() { close(done) }) }()
		go func() {
			select {
			case received := <-signals:
				restore()
				signal.Reset(received)
				if value, ok := received.(syscall.Signal); ok {
					_ = syscall.Kill(os.Getpid(), value)
				}
			case <-done:
			}
		}()
		line, err := bufio.NewReader(io.LimitReader(in, 4097)).ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", errors.New("could not read secret input")
		}
		if _, err := io.WriteString(stderr, "\n"); err != nil {
			return "", errors.New("could not finish secret prompt")
		}
		if len(line) > 4096 {
			return "", errors.New("secret input is larger than 4 KiB")
		}
		if !utf8.Valid(line) {
			return "", errors.New("secret input must be UTF-8 text")
		}
		return string(line), nil
	}
	data, err := io.ReadAll(io.LimitReader(in, 4097))
	if err != nil {
		return "", errors.New("could not read secret input")
	}
	if len(data) > 4096 {
		return "", errors.New("secret input is larger than 4 KiB")
	}
	if !utf8.Valid(data) {
		return "", errors.New("secret input must be UTF-8 text")
	}
	return string(data), nil
}
