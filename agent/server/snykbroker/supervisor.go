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
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

type Supervisor struct {
	sync.Mutex
	output            io.Writer
	fastFailTime      time.Duration
	panicOnMaxRetries bool
	executable        string
	args              []string
	env               map[string]string
	done              chan struct{}
	// firstStart closes once the first child process exists, so Start can
	// promise a live Pid to its caller.
	firstStart     chan struct{}
	firstStartOnce sync.Once
	// outputMu serialises writes to output: a restart can start the next
	// process's line pump before the previous one has drained, and the
	// destination (a bytes.Buffer under test) is not concurrency-safe.
	outputMu sync.Mutex
	// stopFunc, pid and runCount are written by the supervising goroutine and
	// read by whoever called Start, so they are synchronised.
	stopFunc func()
	pid      atomic.Int32
	runCount atomic.Int32
	// lastRunNanos is the child process's own lifetime (exec to exit) for the
	// most recent run.
	lastRunNanos atomic.Int64
	lastError    error
}

func NewSupervisor(
	executable string,
	args []string,
	env map[string]string,
	fastFailTime time.Duration,
) *Supervisor {
	return &Supervisor{
		output:       os.Stdout,
		executable:   executable,
		args:         args,
		env:          env,
		fastFailTime: fastFailTime * 2,
		done:         make(chan struct{}),
		firstStart:   make(chan struct{}),
		stopFunc:     func() {},
	}
}

var errKilled = errors.New("killed")
var errMaxRetries = errors.New("max retries reached")

func (b *Supervisor) Start(maxRetries int, window time.Duration) error {

	if b.runCount.Load() > 0 {
		return errors.New("already started, cannot start again")
	}

	if err := b.runExecutionLoop(maxRetries, window); err != nil {
		return err
	}
	return b.err()
}

// setStopFunc / stopper guard stopFunc, which the supervising goroutine
// replaces as processes come and go while callers may invoke Close at any time.
func (b *Supervisor) setStopFunc(fn func()) {
	b.Lock()
	defer b.Unlock()
	b.stopFunc = fn
}

func (b *Supervisor) stopper() func() {
	b.Lock()
	defer b.Unlock()
	return b.stopFunc
}

func (b *Supervisor) err() error {
	b.Lock()
	defer b.Unlock()
	return b.lastError
}

func (b *Supervisor) runExecutionLoop(maxRetries int, window time.Duration) error {

	tracker := newEventTracker()

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
	b.runCount.Store(1)
	go func() {
		defer close(fastfail)
		for maxRetries > 0 {
			tracker.AddEvent()
			err := b.runCommand()
			// Use the child's own lifetime, not this loop's wall time.
			// Tearing down the pipes and draining the scanners can dwarf
			// fastFailTime on a loaded machine (or under -race), which would
			// otherwise mask a genuinely instant failure.
			runTime := time.Duration(b.lastRunNanos.Load())

			if errors.Is(err, errKilled) {
				finish(nil)
				return
			}

			if err != nil && b.runCount.Load() == 1 && runTime < b.fastFailTime {
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
			b.runCount.Add(1)
		}
	}()

	// Wait for the first process to actually exist before returning. Callers
	// read Pid() straight after Start(), and fastFailTime is a "did it die on
	// the spot" window, not a "process is up" signal — on a loaded machine the
	// fork can easily outlast it, handing back a zero pid.
	select {
	case <-fastfail:
		return b.err()
	case <-b.firstStart:
	}

	// Then the fast-fail observation window proper.
	select {
	case <-fastfail:
	case <-time.After(b.fastFailTime):
	}
	return b.err()
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
			b.writeLine(line)
		}
	}()

	// sigChan triggers shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	running := make(chan struct{})

	go func() {
		<-sigChan
		b.stopper()()
	}()

	var killed atomic.Bool

	// our stopfunc allows anyone to close the process
	// and wait for it to finish
	b.setStopFunc(func() {
		killed.Store(true)

		// if process is running trigger a kill
		if cmd.Process != nil {
			err := cmd.Process.Kill()
			if err != nil {
				fmt.Printf("Error killing process: %v\n", err)
			}
		}

		// wait for it to actually finish
		<-running
	})

	procStart := time.Now()
	err := cmd.Start()

	if err != nil {
		b.lastRunNanos.Store(0)
		return err
	}
	pid := cmd.Process.Pid
	b.pid.Store(int32(pid))
	b.firstStartOnce.Do(func() { close(b.firstStart) })

	// We want to make sure the broker is killed if the agent dies or is killed.  This is
	// mostly useful in the debugger but prevents port from being held open.
	cancelWatchdog := b.runWatchdog(cmd.Process.Pid)
	defer cancelWatchdog()

	// here we wait for it to start then fully finish
	go func() {
		cmd.Wait()
		// Store before closing running: the run loop reads this the moment it
		// unblocks.
		b.lastRunNanos.Store(int64(time.Since(procStart)))
		b.pid.Store(0)
		b.setStopFunc(func() {})
		close(running)
	}()

	// block until the process is done
	<-running
	stopStdOut()
	stopStdErr()
	wg.Wait()

	if killed.Load() {
		err = errKilled
		fmt.Printf("Process %v (pid=%v) killed\n", cmd.Path, pid)
	}

	if err == nil && cmd.ProcessState.ExitCode() != 0 {
		err = fmt.Errorf("command failed with exit code %d", cmd.ProcessState.ExitCode())
	}
	return err
}

func (b *Supervisor) writeLine(line string) {
	b.outputMu.Lock()
	defer b.outputMu.Unlock()
	fmt.Fprintln(b.output, line)
}

func (b *Supervisor) Pid() int {
	return int(b.pid.Load())
}

func (b *Supervisor) Close() error {
	b.stopper()()
	return nil
}

func (b *Supervisor) scanLines(reader io.Reader, output chan string, refCount *sync.WaitGroup) func() {

	// done is set by the returned stop closure on the caller's goroutine and
	// read by the scanning goroutine, so it has to be atomic.
	var done atomic.Bool

	refCount.Add(1)

	// increase buffer size from default of 60K to 1MB
	buffer := make([]byte, 1024*1024)
	go func() {
		for !done.Load() {
			scanner := bufio.NewScanner(reader)
			scanner.Buffer(buffer, cap(buffer)-1)
			for scanner.Scan() {
				ln := scanner.Text()
				output <- ln
				if done.Load() {
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
		done.Store(true)
		refCount.Done()
	}
}
