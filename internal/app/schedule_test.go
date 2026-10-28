package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/config"
	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/transport"
)

// world is a mutable fake host: it samples, signals, and confirms exits
// against one process table.
type world struct {
	mu    sync.Mutex
	procs map[int]domain.ProcessSample
	sent  []string
}

func newWorld(procs ...domain.ProcessSample) *world {
	w := &world{procs: map[int]domain.ProcessSample{}}
	for _, p := range procs {
		w.add(p)
	}
	return w
}

func (w *world) add(p domain.ProcessSample) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p.UID, p.Start = uint32(os.Getuid()), time.Unix(int64(p.PID), 0)
	w.procs[p.PID] = p
}

func (w *world) exit(pid int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.procs, pid)
}

func (w *world) SampleHost(context.Context) (domain.HostSample, error) {
	return domain.HostSample{Time: time.Now(), MemTotal: 100, MemAvailable: 80}, nil
}

func (w *world) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]domain.ProcessSample, 0, len(w.procs))
	for _, p := range w.procs {
		out = append(out, p)
	}
	return out, nil
}

func (w *world) signal(op string, k domain.ProcessKey, uid uint32) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.procs[k.PID]
	if !ok || !p.Start.Equal(k.Start) || p.UID != uid {
		return domain.ErrProcessGone
	}
	w.sent = append(w.sent, fmt.Sprintf("%s:%d", op, k.PID))
	return nil
}

func (w *world) Stop(_ context.Context, k domain.ProcessKey, uid uint32) error {
	return w.signal("stop", k, uid)
}

func (w *world) Continue(_ context.Context, k domain.ProcessKey, uid uint32) error {
	return w.signal("cont", k, uid)
}

func (w *world) Gone(_ context.Context, k domain.ProcessKey) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.procs[k.PID]
	return !ok || !p.Start.Equal(k.Start), nil
}

func (w *world) signals() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.sent, " ")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSchedulerQueuesAndResumes(t *testing.T) {
	for _, mode := range []string{"enforce", "observe"} {
		t.Run(mode, func(t *testing.T) {
			w := newWorld(
				domain.ProcessSample{PID: 10, PPID: 1, Name: "claude"},
				domain.ProcessSample{PID: 11, PPID: 10, Name: "go"},
			)
			cfg := config.Default()
			cfg.SocketPath = filepath.Join(shortTempDir(t), "throtd.sock")
			cfg.SampleInterval = config.Duration(100 * time.Millisecond)
			cfg.Scheduler.Mode, cfg.Scheduler.HeavyLimit, cfg.Scheduler.ResumeCooldown = mode, 1, 0
			ctx, stop := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- NewDaemon(cfg, w, w, w, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(ctx)
			}()
			defer func() { stop(); <-done }()

			status := func() StatusResult {
				var st StatusResult
				transport.Call(context.Background(), cfg.SocketPath, "status", nil, &st)
				return st
			}
			// The scheduler never queues while pressure is unknown (warm-up).
			waitFor(t, "normal pressure", func() bool {
				st := status()
				return len(st.Heavy) == 1 && st.Pressure == domain.PressureNormal
			})

			// A second build starts while the only slot is taken.
			w.add(domain.ProcessSample{PID: 12, PPID: 10, Name: "go"})
			if mode == "observe" {
				waitFor(t, "observed decision", func() bool {
					var events []domain.Event
					transport.Call(context.Background(), cfg.SocketPath, "events", nil, &events)
					for _, e := range events {
						if e.Kind == domain.EventSchedulerDecision && e.Attrs["observed"] == "true" && e.Attrs["pid"] == "12" {
							return true
						}
					}
					return false
				})
				if got := w.signals(); got != "" {
					t.Fatalf("observe mode sent signals: %q", got)
				}
				return
			}
			waitFor(t, "pid 12 queued", func() bool {
				q := status().Scheduler.Queued
				return len(q) == 1 && q[0].PID == 12
			})
			if got := w.signals(); got != "stop:12" {
				t.Fatalf("signals = %q, want stop:12", got)
			}

			// The running build finishes, so the queued one gets the slot.
			w.exit(11)
			waitFor(t, "pid 12 resumed", func() bool { return len(status().Scheduler.Queued) == 0 })
			if got := w.signals(); got != "stop:12 cont:12" {
				t.Fatalf("signals = %q, want stop:12 cont:12", got)
			}
		})
	}
}
