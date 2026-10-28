package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cfcromn/Throt/internal/domain"
)

// recordingSig records every call with its full key. Continue fails for odd
// PIDs so restored records also exercise the resuming path.
type recordingSig struct {
	mu    sync.Mutex
	calls []string
	keys  []domain.ProcessKey
}

func (r *recordingSig) record(op string, k domain.ProcessKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, op)
	r.keys = append(r.keys, k)
}

func (r *recordingSig) Stop(_ context.Context, k domain.ProcessKey, _ uint32) error {
	r.record("stop", k)
	return nil
}

func (r *recordingSig) Continue(_ context.Context, k domain.ProcessKey, _ uint32) error {
	r.record("cont", k)
	if k.PID%2 == 1 {
		return errors.New("boom")
	}
	return nil
}

func (r *recordingSig) Gone(_ context.Context, k domain.ProcessKey) (bool, error) {
	r.record("gone", k)
	return false, nil
}

func (r *recordingSig) Identify(_ context.Context, pid int) (domain.ProcessSample, error) {
	r.record("identify", domain.ProcessKey{PID: pid})
	return domain.ProcessSample{}, domain.ErrProcessGone
}

func FuzzRestoreAndResume(f *testing.F) {
	member := func(pid int, start string) string {
		return fmt.Sprintf(`{"version":1,"paused":[{"process":{"pid":%d,"start":%q},"members":[{"pid":%d,"start":%q}]}]}`, pid, start, pid, start)
	}
	for _, s := range []string{
		member(10, "2026-01-01T00:00:10Z"),
		member(11, "2026-01-01T00:00:11+09:00"),
		member(0, "2026-01-01T00:00:10Z"),
		member(1, "2026-01-01T00:00:10Z"),
		member(10, "0001-01-01T00:00:00Z"),
		member(10, "yesterday"),
		member(10, "2026-01-01T00:00:10Z") + `{}`,
		`{"version":2,"paused":[{"process":{"pid":20,"start":"2026-01-01T00:00:20Z"},"name":"make","owner":"claude-1",` +
			`"members":[{"pid":20,"start":"2026-01-01T00:00:20Z"},{"pid":21,"start":"2026-01-01T00:00:21Z"}],` +
			`"since":"2026-01-01T01:00:00Z","state":"paused","by":"scheduler"}]}`,
		`{"version":2,"paused":[{"members":[],"state":"frozen"}]}`,
		`{"version":1,"paused":[{"members":[],"state":"paused"}]}`,
		`{"version":2,"paused":[{"members":[],"state":"paused"},{"members":[],"state":"paused"}]}`,
		`{"version":1,"paused":[],"extra":true}`,
		`{"version":1,"paused":{}}`,
		`{"version":3,"paused":[]}`,
		`null`, ``, `garbage`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		sig := &recordingSig{}
		c := New(sig, me, clock, nil)
		s, perr := parseState(data)
		errs := c.RestoreAndResume(context.Background(), data)
		if perr != nil {
			if len(errs) != 1 || !errors.Is(errs[0], ErrMalformedState) {
				t.Errorf("malformed state %q: errs = %v", data, errs)
			}
			if len(sig.calls) != 0 || len(c.Paused()) != 0 {
				t.Errorf("malformed state %q signaled %v, recorded %v", data, sig.calls, c.Paused())
			}
			return
		}
		members := make(map[domain.ProcessKey]bool)
		for _, p := range s.Paused {
			for _, m := range p.Members {
				members[m] = true
			}
		}
		for i, k := range sig.keys {
			if sig.calls[i] != "cont" {
				t.Errorf("state %q: restore sent %s to %s", data, sig.calls[i], keyString(k))
			}
			if !members[k] || !validPID(k.PID) {
				t.Errorf("state %q: signaled %v, which it does not list", data, k)
			}
		}
		for _, p := range c.Paused() {
			if p.State != StateResuming {
				t.Errorf("state %q: record %s left in state %q", data, keyString(p.Key), p.State)
			}
			for _, m := range p.Members {
				if !members[m] {
					t.Errorf("state %q: recorded member %v it does not list", data, m)
				}
			}
		}
		for _, err := range errs {
			if errors.Is(err, ErrMalformedState) || !strings.Contains(err.Error(), "control: restore") {
				t.Errorf("state %q: unexpected error %v", data, err)
			}
		}
	})
}
