package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/app"
	"github.com/cfcromn/Throt/internal/domain"
)

// flakyHost fails from its nth call on.
type flakyHost struct {
	calls  *int
	failAt int
}

func (f flakyHost) SampleHost(context.Context) (domain.HostSample, error) {
	*f.calls++
	if *f.calls >= f.failAt {
		return domain.HostSample{}, errors.New("sysctl vm.swapusage failed")
	}
	return domain.HostSample{}, nil
}

// TestCollectHostSampleFails checks both host samples: CPU needs two, so a
// failure of either is reported, and is not an OK check.
func TestCollectHostSampleFails(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		calls := 0
		r := Collect(context.Background(), Inputs{
			Ping:      func(context.Context) (app.PingResult, error) { return app.PingResult{}, errors.New("down") },
			Host:      flakyHost{calls: &calls, failAt: failAt},
			Processes: fakeProcs{},
		})
		if r.Host.OK || r.Host.Error != "sysctl vm.swapusage failed" || calls != failAt {
			t.Errorf("fail at sample %d: host %+v after %d calls", failAt, r.Host, calls)
		}
		// Without a host reading there is nothing to derive CPU for.
		if r.Host.Derived != nil {
			t.Errorf("fail at sample %d: derived %v", failAt, r.Host.Derived)
		}
	}
}

// TestCollectStatusFails checks a daemon that answers ping but not status:
// it is reachable, there is no status section, and the orphan list falls
// back to the local process table.
func TestCollectStatusFails(t *testing.T) {
	r := Collect(context.Background(), Inputs{
		Ping:      func(context.Context) (app.PingResult, error) { return app.PingResult{Version: "v1", Protocol: 1}, nil },
		Status:    func(context.Context) (app.StatusResult, error) { return app.StatusResult{}, errors.New("timeout") },
		Host:      fakeHost{},
		Processes: stoppedProcs{},
		Adapters:  app.Adapters(nil),
		UID:       501,
	})
	if !r.Daemon.Reachable || r.Daemon.Version != "v1" || r.Status != nil {
		t.Errorf("daemon %+v, status %+v", r.Daemon, r.Status)
	}
	if r.Orphans.Source != "local" || r.Orphans.Count != 2 {
		t.Errorf("orphans = %+v, want the local view", r.Orphans)
	}
}

// TestCollectProcessTableFails checks that a process table error is
// reported both as the check and, with throtd down, as the orphan error,
// and that throtd's orphan list is kept when it answered.
func TestCollectProcessTableFails(t *testing.T) {
	down := Collect(context.Background(), Inputs{
		Ping:      func(context.Context) (app.PingResult, error) { return app.PingResult{}, errors.New("down") },
		Host:      fakeHost{},
		Processes: failingProcs{},
	})
	if down.Processes.OK || down.Processes.Error != "ps failed" || down.Orphans.Source != "local" || down.Orphans.Error != "ps failed" || down.Orphans.Count != 0 {
		t.Errorf("down: processes %+v, orphans %+v", down.Processes, down.Orphans)
	}
	up := Collect(context.Background(), Inputs{
		Ping:      func(context.Context) (app.PingResult, error) { return app.PingResult{Version: "v1"}, nil },
		Status:    func(context.Context) (app.StatusResult, error) { return app.StatusResult{OrphanCount: 3}, nil },
		Host:      fakeHost{},
		Processes: failingProcs{},
	})
	if up.Processes.OK || up.Orphans.Source != "throtd" || up.Orphans.Count != 3 || up.Orphans.Error != "" {
		t.Errorf("up: processes %+v, orphans %+v", up.Processes, up.Orphans)
	}
}

// TestCollectRedactsErrors checks that Collect itself redacts the home
// directory in error strings, which routinely carry the socket path.
func TestCollectRedactsErrors(t *testing.T) {
	home := "/home/zq-test-user"
	t.Setenv("HOME", home)
	sock := home + "/run/throt/throtd.sock"
	fail := errors.New("no socket at " + sock)
	r := Collect(context.Background(), Inputs{
		ConfigPath: home + "/.config/throt/config.json",
		SocketPath: sock,
		ConfigErr:  errors.New("config " + home + "/.config/throt/config.json: bad"),
		Ping:       func(context.Context) (app.PingResult, error) { return app.PingResult{}, fail },
		Host:       fakeHost{err: fail},
		Processes:  failingProcsErr{fail},
	})
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), home) {
		t.Errorf("report leaks the home directory: %s", b)
	}
	for _, s := range []string{r.Daemon.Error, r.Host.Error, r.Processes.Error, r.Orphans.Error} {
		if s != "no socket at ~/run/throt/throtd.sock" {
			t.Errorf("error = %q, want the redacted path", s)
		}
	}
	if r.Config != "config ~/.config/throt/config.json: bad" || r.SocketPath != "~/run/throt/throtd.sock" {
		t.Errorf("config %q, socket %q", r.Config, r.SocketPath)
	}
}

type failingProcsErr struct{ err error }

func (f failingProcsErr) SampleProcesses(context.Context) ([]domain.ProcessSample, error) {
	return nil, f.err
}
