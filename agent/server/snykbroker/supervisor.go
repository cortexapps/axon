package snykbroker

import (
	"bufio"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

type Supervisor struct {
	sync.Mutex
	output            io.Writer
	fastFailTime      time.Duration
	panicOnMaxRetries bool

	executable string
	args       []string
	cmd        *exec.Cmd
	env        map[string]string
	done       chan struct{}
	stopFunc   func()
	pid        int
	killed     bool
	runCount   int
	lastError  error
}

func NewSupervisor(
	executable string,
	args []string,
	env map[string]string,
	fastFailTime time.Duration,
) *Supervisor {
	return &Supervisor{
		panicOnMaxRetries: true,
		output:            os.Stdout,
		executable:        executable,
		args:              args,
		env:               env,
		fastFailTime:      fastFailTime * 2,
		done:              make(chan struct{}),
		stopFunc:          func() {},
	}
}

var errKilled = errors.New("killed")
var errMaxRetries = errors.New("max retries reached")

func (b *Supervisor) Start(maxRetries int, window time.Duration) error {

	if b.runCount > 0 {
		return errors.New("already started, cannot start again")
	}

	if err := b.runExecutionLoop(maxRetries, window); err != nil {
		return err
	}
	return b.lastError
}

func (b *Supervisor) runExecutionLoop(maxRetries int, window time.Duration) error {

	tracker := newEventTracker()
	startTime := time.Now()

	finish := func(err error) {
		b.Lock()
		defer b.Unlock()
		b.lastError = err
		if b.done != nil {
			close(b.done)
		}
	}

	fastfail := make(chan struct{})
	// we run this off thread, looping to restart
	// the process if it crashes, but exiting
	// if too many happen in the restart window
	b.runCount = 1
	go func() {
		defer close(fastfail)
		for maxRetries > 0 {
			tracker.AddEvent()
			err := b.runCommand()
			runTime := time.Since(startTime)

			if errors.Is(err, errKilled) {
				finish(nil)
				return
			}

			if err != nil && b.runCount == 1 && runTime < b.fastFailTime {
				finish(fmt.Errorf("run failed immediately: %v", err))
				return
			}

			if err == nil {
				fmt.Printf("Process exited with code 0\n")
			} else {
				fmt.Printf("Process exited with error: %v\n", err)
			}

			if tracker.CountEventsWithinWindow(window) > maxRetries {
				finish(errMaxRetries)
				if b.panicOnMaxRetries {
					panic("max retries reached: " + b.executable)
				}
				return
			}
			b.runCount++
		}
	}()

	// wait to ensure we don't get a fast fail
	select {
	case <-fastfail:
	case <-time.After(b.fastFailTime):
	}
	return b.lastError
}

func (b *Supervisor) Wait() error {
	done := b.done
	if done == nil {
		return b.lastError
	}
	<-done
	return b.lastError
}

//go:embed watchdog.sh
var watchdog string

func (b *Supervisor) runWatchdog(pid int) func() {
	// get this process pid

	// write the watchdog script to a file
	watchdogPath := "/tmp/watchdog.sh"
	err := os.WriteFile(watchdogPath, []byte(watchdog), 0755)
	if err != nil {
		fmt.Println("Error writing watchdog script", zap.Error(err))
		return func() {}
	}

	// run the watchdog script
	cmd := exec.Command(watchdogPath, fmt.Sprintf("%v", os.Getpid()), fmt.Sprintf("%d", pid))
	err = cmd.Start()
	if err != nil {
		fmt.Println("Error running watchdog script", zap.Error(err))
		return func() {}
	}
	return func() {
		cmd.Process.Kill()
	}
}

// runCommand runs a single command and returns the result of the command after it exits,
// or errKilled if it was intentionally shut down.
func (b *Supervisor) runCommand() error {

	cmd := exec.Command(b.executable)
	cmd.Args = append(cmd.Args, b.args...)

	cmd.Env = os.Environ()
	for k, v := range b.env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	output := make(chan string)

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	wg := &sync.WaitGroup{}

	stopStdOut := b.scanLines(stdout, output, wg)
	stopStdErr := b.scanLines(stderr, output, wg)

	go func() {
		for line := range output {
			fmt.Fprintln(b.output, line)
		}
	}()

	// sigChan triggers shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	running := make(chan struct{})

	go func() {
		<-sigChan
		b.stopFunc()
	}()

	killed := false

	// our stopfunc allows anyone to close the process
	// and wait for it to finish
	b.stopFunc = func() {
		killed = true

		// if process is running trigger a kill
		if cmd.Process != nil {
			err := cmd.Process.Kill()
			if err != nil {
				fmt.Printf("Error killing process: %v\n", err)
			}
		}

		// wait for it to actually finish
		<-running
	}

	err := cmd.Start()

	if err != nil {
		return err
	}
	pid := cmd.Process.Pid
	b.pid = pid

	// We want to make sure the broker is killed if the agent dies or is killed.  This is
	// mostly useful in the debugger but prevents port from being held open.
	cancelWatchdog := b.runWatchdog(cmd.Process.Pid)
	defer cancelWatchdog()

	// here we wait for it to start then fully finish
	go func() {
		cmd.Wait()
		b.pid = 0
		b.stopFunc = func() {}
		close(running)
		running = nil
	}()

	// block until the process is done
	<-running
	stopStdOut()
	stopStdErr()
	wg.Wait()

	if killed {
		err = errKilled
		fmt.Printf("Process %v (pid=%v) killed\n", cmd.Path, pid)
	}

	if err == nil && cmd.ProcessState.ExitCode() != 0 {
		err = fmt.Errorf("command failed with exit code %d", cmd.ProcessState.ExitCode())
	}
	return err
}

func (b *Supervisor) Pid() int {
	return b.pid
}

func (b *Supervisor) Close() error {
	b.stopFunc()
	return nil
}

func (b *Supervisor) scanLines(reader io.Reader, output chan string, refCount *sync.WaitGroup) func() {

	done := false

	refCount.Add(1)

	// increase buffer size from default of 60K to 1MB
	buffer := make([]byte, 1024*1024)
	go func() {
		for !done {
			scanner := bufio.NewScanner(reader)
			scanner.Buffer(buffer, cap(buffer)-1)
			for scanner.Scan() {
				ln := scanner.Text()
				output <- ln
				if done {
					return
				}
			}
			err := scanner.Err()

			if err == nil {
				return
			}

			if err == io.EOF {
				return
			}

			output <- fmt.Sprintf("Warning (non-fatal), failed to read from scanner to pipe output: %v", err)

			// dump what we have in the buffer, first 16K, then continue
			output <- string(buffer[0:16*1024]) + "...[END OF BUFFER]"
		}

	}()

	return func() {
		done = true
		refCount.Done()
	}
}
