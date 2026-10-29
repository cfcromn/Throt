//go:build darwin

package platform

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	iostatPath = "/usr/sbin/iostat"
	// iostatMaxAge is how old the latest reading may be (iostat prints one
	// a second) before host CPU falls back to per-process deltas.
	iostatMaxAge = 3 * time.Second
	// iostatRetry is the least time between two starts of iostat, so a
	// missing or failing iostat costs at most one fork a minute.
	iostatRetry = time.Minute
	// iostatMaxLine bounds one output line; real lines are about 30 bytes.
	iostatMaxLine = 256
)

// errIostatNoReading is why host CPU is missing without a fresh reading.
var errIostatNoReading = errors.New("no fresh reading from iostat")

// iostatCPU reads host CPU busy time from the kernel's CPU tick counters
// through one long-lived `iostat -n 0 -w 1` child, the only way to reach
// them without cgo. The child is started on the first reading, owned by
// the sampler and killed by close. A goroutine parses its lines; reading
// never waits for the child. If iostat is missing, exits or prints
// anything unexpected, it is stopped and started again at most once per
// retry period.
type iostatCPU struct {
	path   string
	args   []string
	maxAge time.Duration
	retry  time.Duration

	mu      sync.Mutex
	running bool
	closed  bool
	started time.Time // last start attempt
	cancel  context.CancelFunc
	done    chan struct{} // closed when the reader goroutine has reaped the child
	pid     int
	lastErr error

	last   float64
	lastAt time.Time
	sum    float64 // readings since the previous busy call
	n      int
}

func newIostatCPU() *iostatCPU {
	return &iostatCPU{path: iostatPath, args: []string{"-n", "0", "-w", "1"}, maxAge: iostatMaxAge, retry: iostatRetry}
}

// busy returns the mean busy fraction of the readings since the previous
// call (the latest one when none is new), or false when the latest
// reading is older than maxAge. It starts iostat when it is not running
// and the retry period has passed.
func (c *iostatCPU) busy(now time.Time) (float64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running && !c.closed && (c.started.IsZero() || now.Sub(c.started) >= c.retry) {
		c.start(now)
	}
	if c.lastAt.IsZero() || now.Sub(c.lastAt) > c.maxAge {
		c.sum, c.n = 0, 0
		if c.lastErr != nil {
			return 0, false, fmt.Errorf("%w: %w", errIostatNoReading, c.lastErr)
		}
		return 0, false, errIostatNoReading
	}
	v := c.last
	if c.n > 0 {
		v = c.sum / float64(c.n)
	}
	c.sum, c.n = 0, 0
	return v, true, nil
}

// start runs iostat; c.mu is held.
func (c *iostatCPU) start(now time.Time) {
	c.started = now
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, c.path, c.args...)
	cmd.Env = []string{"LC_ALL=C"}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Second // then SIGKILL and close the pipe
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cancel()
		c.lastErr = fmt.Errorf("start iostat: %w", err)
		return
	}
	c.running, c.cancel, c.done, c.pid = true, cancel, make(chan struct{}), cmd.Process.Pid
	go c.read(cmd, stdout, cancel, c.done)
}

// read parses iostat's lines until it exits or prints something
// unexpected, then stops and reaps it.
func (c *iostatCPU) read(cmd *exec.Cmd, stdout io.Reader, cancel context.CancelFunc, done chan struct{}) {
	defer close(done)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, iostatMaxLine), iostatMaxLine)
	var err error
	first := true
	for sc.Scan() {
		v, header, perr := parseIostatLine(sc.Text())
		if perr != nil {
			err = perr
			break
		}
		if header {
			continue
		}
		if first { // averaged since boot
			first = false
			continue
		}
		c.mu.Lock()
		c.last, c.lastAt = v, time.Now()
		c.sum += v
		c.n++
		c.mu.Unlock()
	}
	if err == nil {
		err = sc.Err()
	}
	cancel()
	waitErr := cmd.Wait()
	if err == nil && waitErr != nil {
		err = fmt.Errorf("iostat: %w", waitErr)
	} else if err == nil {
		err = errors.New("iostat exited")
	}
	c.mu.Lock()
	c.running, c.lastErr = false, err
	c.mu.Unlock()
}

// close stops iostat and waits for the reader goroutine. Later busy calls
// never start it again.
func (c *iostatCPU) close() {
	c.mu.Lock()
	c.closed = true
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
