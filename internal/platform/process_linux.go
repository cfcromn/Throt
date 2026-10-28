//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// NewProcessSampler returns a process sampler for the current OS. On Linux
// it reads /proc/<pid>/stat and /proc/<pid>/status for every numeric entry
// in /proc.
func NewProcessSampler() ProcessSampler { return linuxProcessSampler{} }

type linuxProcessSampler struct{}

func (linuxProcessSampler) SampleProcesses(ctx context.Context) ([]domain.ProcessSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	var boot time.Time
	if err := withFile("/proc/stat", func(r io.Reader) (err error) {
		boot, err = parseBootTime(r)
		return err
	}); err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	pageSize := uint64(os.Getpagesize())
	out := make([]domain.ProcessSample, 0, len(entries))
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil || !e.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("sample processes: %w", err)
		}
		p, err := readLinuxProcess("/proc/"+e.Name(), boot, pageSize)
		if err != nil {
			// Usually ENOENT/ESRCH because the process exited mid-scan; any
			// other per-process failure also just means it is not visible.
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("sample processes: /proc listed no readable processes")
	}
	return out, nil
}

// readLinuxProcess reads one process directory such as /proc/123.
func readLinuxProcess(dir string, boot time.Time, pageSize uint64) (domain.ProcessSample, error) {
	b, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return domain.ProcessSample{}, err
	}
	st, err := parsePIDStat(b)
	if err != nil {
		return domain.ProcessSample{}, err
	}
	var uid uint32
	if err := withFile(dir+"/status", func(r io.Reader) (err error) {
		uid, err = parseStatusUID(r)
		return err
	}); err != nil {
		return domain.ProcessSample{}, err
	}
	return domain.ProcessSample{
		PID:     st.PID,
		PPID:    st.PPID,
		UID:     uid,
		Start:   boot.Add(ticksToDuration(st.StartTime)),
		Name:    st.Comm,
		CPUTime: ticksToDuration(st.CPUTicks),
		RSS:     st.RSSPages * pageSize,
	}, nil
}
