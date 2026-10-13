package launch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

type childExit struct{ status int }

func (e *childExit) Error() string { return "vendor process exited" }

// Endpoint launches retain a parent to remove their private settings file.
// The child inherits the terminal and working directory. Forward termination
// signals addressed to the parent and always wait before settings cleanup.
func supervisedLaunch(executable string, args, environment []string) error {
	command := exec.Command(executable, args...)
	command.Env = environment
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return fmt.Errorf("could not launch the endpoint CLI")
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	var forwardingErr error
	go func() {
		defer close(stopped)
		for {
			select {
			case received := <-signals:
				if err := command.Process.Signal(received); err != nil && !errors.Is(err, os.ErrProcessDone) {
					forwardingErr = err
				}
			case <-done:
				return
			}
		}
	}()
	err := command.Wait()
	close(done)
	<-stopped
	if err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			status := exited.ExitCode()
			if wait, ok := exited.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
				status = 128 + int(wait.Signal())
			}
			return &childExit{status}
		}
		return fmt.Errorf("could not wait for the endpoint CLI")
	}
	if forwardingErr != nil {
		return fmt.Errorf("could not forward a signal to the endpoint CLI")
	}
	return nil
}
