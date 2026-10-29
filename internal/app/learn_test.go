package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
)

var learnT0 = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// heavyAt is a heavy workload of agent whose top process started at start.
func heavyAt(pid, agent int, start time.Time, rss uint64) process.WorkloadView {
	return process.WorkloadView{Key: domain.ProcessKey{PID: pid, Start: start}, Name: "zsh", Class: domain.ClassCommand, OwnerPID: agent, RSS: rss}
}

func TestLearnerLearnsOneReport(t *testing.T) {
	l := newLearner()
	if l.expect(10, "python train.py", false, learnT0, learnT0) {
		t.Fatal("unknown signature is heavy")
	}
	// The shell starts a second later and becomes heavy three samples on.
	got := l.observe(learnT0.Add(6*time.Second), []process.WorkloadView{heavyAt(50, 10, learnT0.Add(time.Second), 1<<30)})
	if len(got) != 1 || got[0] != "python train.py" {
		t.Fatalf("learned %q", got)
	}
	if !l.expect(10, "python train.py", false, learnT0, learnT0.Add(time.Minute)) {
		t.Error("learned signature is not heavy")
	}
	// Learned signatures are not per agent instance: agents come and go.
	if !l.expect(11, "python train.py", false, learnT0, learnT0.Add(time.Minute)) {
		t.Error("learned signature is not heavy for another agent")
	}
	e := l.list()[0]
	if e.HeavyRuns != 1 || e.PeakRSS != 1<<30 || !e.LastHeavy.Equal(learnT0.Add(6*time.Second)) || !l.dirty {
		t.Errorf("entry %+v, dirty %v", e, l.dirty)
	}
}

func TestLearnerWindow(t *testing.T) {
	tests := []struct {
		name  string
		agent int
		start time.Duration // of the workload, after the report
		want  bool
	}{
		{"just after", 10, 0, true},
		{"truncated start", 10, -learnSlack, true},
		{"before the reporter", 10, -learnSlack - time.Second, false},
		{"end of window", 10, learnWindow, true},
		{"after the window", 10, learnWindow + time.Second, false},
		{"another agent", 11, time.Second, false},
		{"unattributed", 0, time.Second, false},
	}
	for _, tt := range tests {
		l := newLearner()
		l.expect(10, "pnpm test:e2e", false, learnT0, learnT0)
		got := l.observe(learnT0.Add(learnWindow+5*time.Second), []process.WorkloadView{heavyAt(50, tt.agent, learnT0.Add(tt.start), 1)})
		if (len(got) == 1) != tt.want {
			t.Errorf("%s: learned %q, want %v", tt.name, got, tt.want)
		}
	}
}

func TestLearnerWaitedReportExtendsWindow(t *testing.T) {
	l := newLearner()
	// The hook reports, waits 40s for capacity, and reports again; the
	// command starts after the second report.
	l.expect(10, "npx playwright", false, learnT0, learnT0)
	l.expect(10, "npx playwright", false, learnT0.Add(40*time.Second), learnT0.Add(40*time.Second))
	got := l.observe(learnT0.Add(45*time.Second), []process.WorkloadView{heavyAt(50, 10, learnT0.Add(41*time.Second), 1)})
	if len(got) != 1 || len(l.pending) != 0 {
		t.Errorf("learned %q, %d pending", got, len(l.pending))
	}
	// throt run reports once, at exec: its own start opens the window.
	l = newLearner()
	l.expect(10, "build.sh", false, learnT0, learnT0.Add(3*time.Minute))
	if got := l.observe(learnT0.Add(3*time.Minute), []process.WorkloadView{heavyAt(50, 10, learnT0, 1)}); len(got) != 1 {
		t.Errorf("throt run: learned %q", got)
	}
}

func TestLearnerAmbiguous(t *testing.T) {
	l := newLearner()
	l.expect(10, "python a.py", false, learnT0, learnT0)
	l.expect(10, "python b.py", false, learnT0, learnT0.Add(time.Second))
	if got := l.observe(learnT0.Add(5*time.Second), []process.WorkloadView{heavyAt(50, 10, learnT0.Add(2*time.Second), 1)}); len(got) != 0 {
		t.Errorf("learned %q from two reports", got)
	}
	if len(l.pending) != 0 || len(l.learned) != 0 {
		t.Errorf("%d pending, %d learned after an ambiguous match", len(l.pending), len(l.learned))
	}
	// A command heavy by name is never learned, and still makes an
	// unknown one beside it ambiguous.
	l.expect(10, "make", true, learnT0, learnT0.Add(time.Minute))
	if got := l.observe(learnT0.Add(62*time.Second), []process.WorkloadView{heavyAt(51, 10, learnT0.Add(61*time.Second), 1)}); len(got) != 0 {
		t.Errorf("learned named %q", got)
	}
	l.expect(10, "make", true, learnT0, learnT0.Add(2*time.Minute))
	l.expect(10, "python c.py", false, learnT0, learnT0.Add(2*time.Minute))
	if got := l.observe(learnT0.Add(125*time.Second), []process.WorkloadView{heavyAt(52, 10, learnT0.Add(121*time.Second), 1)}); len(got) != 0 {
		t.Errorf("learned %q beside a named command", got)
	}
	// Reports of other agents do not count.
	l.expect(10, "python d.py", false, learnT0, learnT0.Add(3*time.Minute))
	l.expect(11, "python e.py", false, learnT0, learnT0.Add(3*time.Minute))
	if got := l.observe(learnT0.Add(185*time.Second), []process.WorkloadView{heavyAt(53, 10, learnT0.Add(181*time.Second), 1)}); len(got) != 1 || got[0] != "python d.py" {
		t.Errorf("learned %q, want python d.py", got)
	}
}

func TestLearnerSeesAWorkloadOnce(t *testing.T) {
	l := newLearner()
	w := heavyAt(50, 10, learnT0.Add(time.Second), 1<<20)
	l.expect(10, "a", false, learnT0, learnT0)
	l.observe(learnT0.Add(2*time.Second), []process.WorkloadView{w})
	// A second report in the window matches no workload already seen.
	l.expect(10, "b", false, learnT0, learnT0.Add(3*time.Second))
	if got := l.observe(learnT0.Add(4*time.Second), []process.WorkloadView{w}); len(got) != 0 {
		t.Errorf("learned %q from a workload already matched", got)
	}
	// Its memory keeps raising the peak.
	w.RSS = 3 << 20
	l.dirty = false
	l.observe(learnT0.Add(5*time.Second), []process.WorkloadView{w})
	if e := l.learned["a"]; e.PeakRSS != 3<<20 || !l.dirty {
		t.Errorf("peak %d, dirty %v", e.PeakRSS, l.dirty)
	}
	w.RSS = 3<<20 + 1
	l.dirty = false
	l.observe(learnT0.Add(6*time.Second), []process.WorkloadView{w})
	if e := l.learned["a"]; e.PeakRSS != 3<<20+1 || l.dirty {
		t.Errorf("small growth: peak %d, dirty %v", e.PeakRSS, l.dirty)
	}
	// Once gone it is forgotten, so the maps stay bounded.
	l.observe(learnT0.Add(7*time.Second), nil)
	if len(l.seen) != 0 || len(l.tracked) != 0 {
		t.Errorf("seen %d, tracked %d", len(l.seen), len(l.tracked))
	}
}

func TestLearnerForgetsLightRuns(t *testing.T) {
	l := newLearner()
	now := learnT0
	heavyRun := func() {
		l.expect(10, "node build.js", false, now, now)
		l.observe(now.Add(2*time.Second), []process.WorkloadView{heavyAt(int(now.Unix()%1000)+2, 10, now.Add(time.Second), 1)})
		now = now.Add(time.Hour)
	}
	lightRun := func() {
		l.expect(10, "node build.js", false, now, now)
		l.observe(now.Add(2*time.Second), nil)
		l.observe(now.Add(pendingTTL), nil)
		now = now.Add(time.Hour)
	}
	heavyRun()
	lightRun()
	lightRun()
	heavyRun() // resets the count
	lightRun()
	lightRun()
	if _, ok := l.learned["node build.js"]; !ok {
		t.Fatal("forgotten after two light runs in a row")
	}
	lightRun()
	if _, ok := l.learned["node build.js"]; ok {
		t.Fatal("still learned after three light runs in a row")
	}
	// A pending report also ages out without a learned signature.
	l.expect(10, "x", false, now, now)
	l.observe(now.Add(pendingTTL), nil)
	if len(l.pending) != 0 || len(l.learned) != 0 {
		t.Errorf("%d pending, %d learned", len(l.pending), len(l.learned))
	}
}

func TestLearnerBounds(t *testing.T) {
	l := newLearner()
	for i := range maxLearned + 1 {
		now := learnT0.Add(time.Duration(i) * time.Minute)
		sig := fmt.Sprintf("job%d", i)
		l.expect(10, sig, false, now, now)
		if got := l.observe(now.Add(time.Second), []process.WorkloadView{heavyAt(100+i, 10, now, 1)}); len(got) != 1 {
			t.Fatalf("%s: learned %q", sig, got)
		}
	}
	if len(l.learned) != maxLearned {
		t.Fatalf("%d learned, want %d", len(l.learned), maxLearned)
	}
	if _, ok := l.learned["job0"]; ok {
		t.Error("the signature heavy longest ago was kept")
	}
	if _, ok := l.learned[fmt.Sprintf("job%d", maxLearned)]; !ok {
		t.Error("the newest signature was not kept")
	}
	for i := range maxPending + 10 {
		l.expect(10, fmt.Sprintf("p%d", i), false, learnT0, learnT0)
	}
	if len(l.pending) != maxPending || l.pending[0].sig != "p10" {
		t.Errorf("%d pending, oldest %q", len(l.pending), l.pending[0].sig)
	}
	// Unattributed callers are answered but never recorded.
	l = newLearner()
	l.expect(0, "x", false, learnT0, learnT0)
	if len(l.pending) != 0 {
		t.Error("unattributed report recorded")
	}
}

func TestLearnerPersistence(t *testing.T) {
	l := newLearner()
	l.expect(10, "python train.py", false, learnT0, learnT0)
	l.observe(learnT0.Add(time.Second), []process.WorkloadView{heavyAt(50, 10, learnT0, 5<<30)})
	data, err := l.marshal()
	if err != nil {
		t.Fatal(err)
	}
	m := newLearner()
	if err := m.load(data); err != nil || m.dirty {
		t.Fatalf("load: %v, dirty %v", err, m.dirty)
	}
	if got, want := fmt.Sprint(m.list()), fmt.Sprint(l.list()); got != want {
		t.Errorf("round trip %s, want %s", got, want)
	}

	for name, in := range map[string]string{
		"not json":      "{",
		"wrong version": `{"version": 2, "signatures": []}`,
		"no version":    `{"signatures": []}`,
		"wrong type":    `{"version": 1, "signatures": {}}`,
		"too large":     `{"version": 1, "signatures": [], "x": "` + strings.Repeat("a", maxLearnedFile) + `"}`,
	} {
		m := newLearner()
		m.learned["keep"] = &LearnedSignature{Signature: "keep"}
		if err := m.load([]byte(in)); err == nil || !m.dirty || len(m.learned) != 1 {
			t.Errorf("%s: err %v, dirty %v, %d learned", name, err, m.dirty, len(m.learned))
		}
	}
	// Invalid entries are skipped, and the file rewritten without them.
	m = newLearner()
	in := `{"version": 1, "signatures": [
		{"signature": "ok one", "heavy_runs": 1},
		{"signature": "ok one", "heavy_runs": 2},
		{"signature": "bad sig/x", "heavy_runs": 1},
		{"signature": "never", "heavy_runs": 0},
		{"signature": "missed", "heavy_runs": 1, "misses": 3},
		{"signature": "neg", "heavy_runs": 1, "misses": -1}]}`
	if err := m.load([]byte(in)); err != nil || !m.dirty || len(m.learned) != 1 || m.learned["ok one"] == nil {
		t.Errorf("err %v, dirty %v, learned %v", err, m.dirty, m.list())
	}
}

func FuzzLearnerLoad(f *testing.F) {
	f.Add([]byte(`{"version":1,"signatures":[{"signature":"python train.py","peak_rss":1,"heavy_runs":2,"misses":1,"last_heavy":"2026-10-29T12:00:00Z"}]}`))
	f.Add([]byte(`{"version":1,"signatures":null}`))
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		l := newLearner()
		if err := l.load(data); err != nil {
			return
		}
		if len(l.learned) > maxLearned {
			t.Fatalf("%d learned", len(l.learned))
		}
		out, err := l.marshal()
		if err != nil {
			t.Fatal(err)
		}
		m := newLearner()
		if err := m.load(out); err != nil || m.dirty || len(m.learned) != len(l.learned) {
			t.Fatalf("reload of %s: %v, dirty %v", out, err, m.dirty)
		}
	})
}

// learnDaemon is a leaseDaemon whose state directory is a temp dir.
func learnDaemon(t *testing.T) (*Daemon, *fakeIdent) {
	t.Helper()
	d, ident := leaseDaemon(t, oneSlot(2))
	d.cfg.SocketPath = filepath.Join(t.TempDir(), "throtd.sock")
	return d, ident
}

func expectAs(t *testing.T, d *Daemon, p ExpectParams) (ExpectResult, error) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.expect(context.Background(), raw)
	if err != nil {
		return ExpectResult{}, err
	}
	return v.(ExpectResult), nil
}

func TestExpectMethod(t *testing.T) {
	d, ident := learnDaemon(t)
	// The hook (300) runs under a shell (200) the agent (10) started.
	ident.procs[300] = child(200)
	ident.procs[200] = child(10)
	r, err := expectAs(t, d, ExpectParams{PID: 300, Signature: "python train.py"})
	if err != nil || r.Heavy {
		t.Fatalf("expect: %+v, %v", r, err)
	}
	if len(d.learn.pending) != 1 || d.learn.pending[0].agent != 10 || !d.learn.pending[0].since.Equal(time.Unix(1, 0)) {
		t.Fatalf("pending %+v", d.learn.pending)
	}
	d.learnFrom(time.Now(), []process.WorkloadView{heavyAt(400, 10, time.Now(), 2<<30)})
	if r, err := expectAs(t, d, ExpectParams{PID: 300, Signature: "python train.py"}); err != nil || !r.Heavy {
		t.Errorf("after learning: %+v, %v", r, err)
	}
	// learnFrom saved it, privately.
	fi, err := os.Stat(d.learnedPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("learned.json: %v, %v", fi, err)
	}
	for _, p := range []ExpectParams{{PID: 1, Signature: "a"}, {PID: 300, Signature: ""}, {PID: 300, Signature: "a b c"}, {PID: 300, Signature: "cat /etc/passwd"}} {
		if _, err := expectAs(t, d, p); err == nil || !strings.Contains(err.Error(), "invalid params") {
			t.Errorf("expect(%+v): %v", p, err)
		}
	}
	ident.procs[301] = domain.ProcessSample{PPID: 10, UID: uint32(os.Getuid()) + 1, Start: time.Unix(1, 0)}
	if _, err := expectAs(t, d, ExpectParams{PID: 301, Signature: "a"}); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Errorf("foreign caller: %v", err)
	}
	// Refusals are not control events: the hook asks before every command.
	if n := len(eventsOf(d, domain.EventControlRefused)); n != 0 {
		t.Errorf("%d control_refused events", n)
	}
	// Signatures never reach the event log.
	for _, e := range d.events.Recent(0) {
		if strings.Contains(fmt.Sprint(e), "train") {
			t.Errorf("event %+v carries the signature", e)
		}
	}
}

func TestLearnedMethodAndRestart(t *testing.T) {
	d, _ := learnDaemon(t)
	for _, sig := range []string{"a", "b", "c"} {
		d.learn.heavyRun(sig, 1, learnT0)
	}
	d.saveLearned()
	call := func(p LearnedParams) LearnedResult {
		raw, _ := json.Marshal(p)
		v, err := d.learnedSignatures(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return v.(LearnedResult)
	}
	if r := call(LearnedParams{}); len(r.Signatures) != 3 || r.Forgotten != 0 {
		t.Errorf("list: %+v", r)
	}
	if r := call(LearnedParams{Forget: "b"}); len(r.Signatures) != 2 || r.Forgotten != 1 {
		t.Errorf("forget: %+v", r)
	}
	if r := call(LearnedParams{Forget: "zz"}); r.Forgotten != 0 {
		t.Errorf("forget unknown: %+v", r)
	}
	// A restarted daemon loads what was saved.
	d2, _ := leaseDaemon(t, oneSlot(2))
	d2.cfg.SocketPath = d.cfg.SocketPath
	d2.loadLearned()
	if got := d2.learn.list(); len(got) != 2 || got[0].Signature != "a" || got[1].Signature != "c" {
		t.Errorf("after restart: %+v", got)
	}
	if r := call(LearnedParams{Clear: true}); len(r.Signatures) != 0 || r.Forgotten != 2 {
		t.Errorf("clear: %+v", r)
	}
	d3, _ := leaseDaemon(t, oneSlot(2))
	d3.cfg.SocketPath = d.cfg.SocketPath
	d3.loadLearned()
	if n := len(d3.learn.learned); n != 0 {
		t.Errorf("%d learned after clear and restart", n)
	}
}

func TestMalformedLearnedFileIsReplaced(t *testing.T) {
	d, _ := learnDaemon(t)
	if err := os.WriteFile(d.learnedPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.loadLearned()
	if len(d.learn.learned) != 0 || !d.learn.dirty {
		t.Fatalf("learned %d, dirty %v", len(d.learn.learned), d.learn.dirty)
	}
	d.learnFrom(learnT0, nil)
	b, err := os.ReadFile(d.learnedPath())
	if err != nil || string(b) != `{"version":1,"signatures":[]}` {
		t.Errorf("replaced with %q, %v", b, err)
	}
	// A failed write is retried at the next save.
	d.learn.heavyRun("x", 1, learnT0)
	d.writeFile = func(string, []byte) error { return os.ErrPermission }
	d.saveLearned()
	if !d.learn.dirty {
		t.Error("failed save cleared dirty")
	}
	d.writeFile = writeFileAtomic
	d.saveLearned()
	if b, _ := os.ReadFile(d.learnedPath()); !strings.Contains(string(b), `"x"`) {
		t.Errorf("not saved after retry: %s", b)
	}
}
