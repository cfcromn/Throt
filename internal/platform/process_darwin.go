//go:build darwin

package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// psPath is invoked directly, never through a shell.
const psPath = "/bin/ps"

// maxPSOutput bounds how much ps output is read.
const maxPSOutput = 32 << 20

// NewProcessSampler returns a process sampler for the current OS. On macOS
// it runs /bin/ps: libproc would need cgo. The caller's context deadline
// bounds how long ps may run.
func NewProcessSampler() ProcessSampler { return darwinProcessSampler{} }

type darwinProcessSampler struct{}

func (darwinProcessSampler) SampleProcesses(ctx context.Context) ([]domain.ProcessSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	cmd := exec.CommandContext(ctx, psPath, "-axww", "-o", psColumns)
	// The C locale fixes the lstart format; TZ is kept so ps and Go agree on
	// local time. Nothing else from the environment is passed on.
	cmd.Env = []string{"LC_ALL=C"}
	if tz, ok := os.LookupEnv("TZ"); ok {
		cmd.Env = append(cmd.Env, "TZ="+tz)
	}
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sample processes: start ps: %w", err)
	}
	lr := &io.LimitedReader{R: stdout, N: maxPSOutput + 1}
	procs, parseErr := parsePS(lr, time.Local)
	overflow := lr.N == 0
	if overflow || parseErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("sample processes: %w", ctx.Err())
	case overflow:
		return nil, fmt.Errorf("sample processes: ps output exceeds %d bytes", maxPSOutput)
	case parseErr != nil:
		return nil, fmt.Errorf("sample processes: %w", parseErr)
	case waitErr != nil:
		return nil, fmt.Errorf("sample processes: ps: %w", waitErr)
	case len(procs) == 0:
		return nil, errors.New("sample processes: ps listed no processes")
	}
	return procs, nil
}
