//go:build darwin

package platform

import (
	"bytes"
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
	// Two passes: the stats pass has no attacker-controlled text and defines
	// the process set; names come from a separate pass (see parsePSNames).
	stats, err := runPS(ctx, psStatColumns)
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	names, err := runPS(ctx, psNameColumns)
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	procs, err := parsePS(bytes.NewReader(stats), bytes.NewReader(names), time.Local)
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	if len(procs) == 0 {
		return nil, errors.New("sample processes: ps listed no processes")
	}
	return procs, nil
}

// runPS runs ps for all processes with the given -o columns and returns its
// bounded output.
func runPS(ctx context.Context, columns string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, psPath, "-axww", "-o", columns)
	// The C locale fixes the lstart format; TZ is kept so ps and Go agree on
	// local time. Nothing else from the environment is passed on.
	cmd.Env = []string{"LC_ALL=C"}
	if tz, ok := os.LookupEnv("TZ"); ok {
		cmd.Env = append(cmd.Env, "TZ="+tz)
	}
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ps: %w", err)
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxPSOutput+1))
	overflow := len(out) > maxPSOutput
	if overflow || readErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case overflow:
		return nil, fmt.Errorf("ps output exceeds %d bytes", maxPSOutput)
	case readErr != nil:
		return nil, fmt.Errorf("read ps output: %w", readErr)
	case waitErr != nil:
		return nil, fmt.Errorf("ps: %w", waitErr)
	}
	return out, nil
}
