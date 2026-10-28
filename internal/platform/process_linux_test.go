//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// referenceSample is the sampler before the per-process cache: os.ReadDir,
// then os.ReadFile of stat and a scan of status for every PID, with the
// reference parsers. The sampler must return exactly what it returns.
func referenceSample(root string, boot time.Time) ([]domain.ProcessSample, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []domain.ProcessSample
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil || !e.IsDir() {
			continue
		}
		p, err := referenceReadProcess(root+"/"+e.Name(), boot)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// referenceIdentify is Identify before the change, error mapping included.
func referenceIdentify(root string, pid int, boot time.Time) (domain.ProcessSample, error) {
	p, err := referenceReadProcess(root+"/"+strconv.Itoa(pid), boot)
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH):
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, domain.ErrProcessGone)
	case err != nil:
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: %w", pid, err)
	case p.PID != pid:
		return domain.ProcessSample{}, fmt.Errorf("identity of pid %d: stat reports pid %d", pid, p.PID)
	}
	return p, nil
}

func referenceReadProcess(dir string, boot time.Time) (domain.ProcessSample, error) {
	b, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return domain.ProcessSample{}, err
	}
	st, err := referenceParsePIDStat(b)
	if err != nil {
		return domain.ProcessSample{}, err
	}
	var uid uint32
	if err := withFile(dir+"/status", func(r io.Reader) (err error) {
		uid, err = referenceStatusUID(r)
		return err
	}); err != nil {
		return domain.ProcessSample{}, err
	}
	return domain.ProcessSample{
		PID: st.PID, PPID: st.PPID, UID: uid,
		Start:   boot.Add(ticksToDuration(st.StartTime)),
		Name:    st.Comm,
		CPUTime: ticksToDuration(st.CPUTicks),
		RSS:     st.RSSPages * uint64(os.Getpagesize()),
		State:   st.State,
	}, nil
}

// fixtureProc is one process of a fixture /proc tree.
type fixtureProc struct {
	pid, ppid    int
	comm         string
	state        string
	utime, start uint64
	rss          uint64
	uid          uint32
}

func (p fixtureProc) write(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(p.pid))
	stat := fmt.Sprintf("%d (%s) %s %d %d %d 0 -1 4194560 100 0 0 0 %d 7 0 0 20 0 1 0 %d 1000000 %d 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n",
		p.pid, p.comm, p.state, p.ppid, p.pid, p.pid, p.utime, p.start, p.rss)
	status := fmt.Sprintf("Name:\t%s\nUmask:\t0022\nState:\t%s\nTgid:\t%d\nPid:\t%d\nPPid:\t%d\nUid:\t%d\t%d\t%d\t%d\nGid:\t0\t0\t0\t0\n",
		p.comm, p.state, p.pid, p.pid, p.ppid, p.uid, p.uid, p.uid, p.uid)
	writeFile(t, dir, "stat", stat)
	writeFile(t, dir, "status", status)
}

// TestSampleProcessesMatchesReference runs the sampler and Identify over a
// fixture tree through rounds of changes (CPU, state, exec, PID reuse,
// exits and new processes, unreadable and malformed entries) and requires
// byte-identical results to the reference on every sample.
func TestSampleProcessesMatchesReference(t *testing.T) {
	root := t.TempDir()
	boot := time.Unix(1700000000, 0)
	clock := &bootClock{boot: boot}
	s := &linuxProcessSampler{boot: clock, root: root}
	ctl := linuxProcessController{boot: clock, root: root}

	procs := map[int]fixtureProc{}
	set := func(p fixtureProc) { procs[p.pid] = p; p.write(t, root) }
	for _, p := range []fixtureProc{
		{pid: 1, comm: "init", state: "S", start: 1, uid: 0},
		{pid: 2, ppid: 1, comm: "bash", state: "S", start: 50, uid: 1000, utime: 3, rss: 900},
		{pid: 10, ppid: 2, comm: "my prog", state: "R", start: 70, uid: 1000},
		{pid: 100, ppid: 2, comm: "a) b (c", state: "S", start: 90, uid: 1001},
		{pid: 4242, ppid: 2, comm: "new\nline\x7f", state: "Z", start: 95, uid: 1000},
		{pid: 4243, ppid: 2, comm: "naïve", state: "T", start: 96, uid: 1000},
		{pid: 4244, ppid: 2, comm: "", state: "t", start: 97, uid: 4294967295},
	} {
		set(p)
	}
	// Entries the sampler must skip as the reference does.
	if err := os.Mkdir(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("1", filepath.Join(root, "self")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "77", "not a directory")
	set(fixtureProc{pid: 88, comm: "nostatus", state: "S", start: 5})
	if err := os.Remove(filepath.Join(root, "88", "status")); err != nil {
		t.Fatal(err)
	}
	set(fixtureProc{pid: 89, comm: "badstat", state: "S", start: 5})
	writeFile(t, filepath.Join(root, "89"), "stat", "89 (badstat) S 1 2 3\n")
	set(fixtureProc{pid: 90, comm: "nouid", state: "S", start: 5})
	writeFile(t, filepath.Join(root, "90"), "status", "Name:\tnouid\n")
	// A status larger than the initial buffer, with Uid last.
	set(fixtureProc{pid: 91, comm: "groups", state: "S", start: 5, uid: 7})
	writeFile(t, filepath.Join(root, "91"), "status", "Groups:\t"+strings.Repeat("1000 ", 3000)+"\nUid:\t7\t7\t7\t7\n")

	check := func(round string) {
		t.Helper()
		for i := range 2 * uidRefresh { // every refresh turn, cached and not
			got, err := s.SampleProcesses(context.Background())
			if err != nil {
				t.Fatalf("%s: %v", round, err)
			}
			want, err := referenceSample(root, boot)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s, sample %d:\n got %+v\nwant %+v", round, i, got, want)
			}
		}
		for _, pid := range []int{1, 2, 10, 100, 101, 4242, 4243, 4244, 77, 88, 89, 90, 91, 999} {
			got, gotErr := ctl.Identify(context.Background(), pid)
			want, wantErr := referenceIdentify(root, pid, boot)
			if got != want || errString(gotErr) != errString(wantErr) || errors.Is(gotErr, domain.ErrProcessGone) != errors.Is(wantErr, domain.ErrProcessGone) {
				t.Errorf("%s: Identify(%d) = %+v, %v; reference %+v, %v", round, pid, got, gotErr, want, wantErr)
			}
		}
	}
	check("initial")

	p := procs[2]
	p.utime, p.rss, p.state = 500, 1200, "T"
	set(p)
	p = procs[10]
	p.comm = "make" // exec: same PID and start, new comm
	set(p)
	check("cpu, state and exec")

	set(fixtureProc{pid: 10, ppid: 1, comm: "reused", state: "S", start: 400, uid: 2000}) // PID reuse
	check("pid reuse")

	if err := os.RemoveAll(filepath.Join(root, "100")); err != nil {
		t.Fatal(err)
	}
	set(fixtureProc{pid: 101, ppid: 2, comm: "later", state: "D", start: 410, uid: 1000})
	check("exit and new")
}

// TestSampleProcessesUIDRefresh changes a process's uid in place, as
// setuid(2) does, and requires the sampler to report it within uidRefresh
// samples.
func TestSampleProcessesUIDRefresh(t *testing.T) {
	root := t.TempDir()
	s := &linuxProcessSampler{boot: &bootClock{boot: time.Unix(1700000000, 0)}, root: root}
	p := fixtureProc{pid: 1234, comm: "sshd", state: "S", start: 9, uid: 0}
	p.write(t, root)
	if _, err := s.SampleProcesses(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.uid = 1000
	p.write(t, root)
	for i := range uidRefresh {
		got, err := s.SampleProcesses(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got[0].UID == 1000 {
			return
		}
		t.Logf("sample %d: uid %d", i+1, got[0].UID)
	}
	t.Fatalf("uid change not seen within %d samples", uidRefresh)
}

// TestSampleProcessesMatchesReferenceOnProc compares the sampler with the
// reference on the real /proc. Processes can start, exit or run between the
// two reads, so only the identities of processes in both are compared.
func TestSampleProcessesMatchesReferenceOnProc(t *testing.T) {
	clock := &bootClock{}
	s := &linuxProcessSampler{boot: clock}
	if _, err := s.SampleProcesses(context.Background()); err != nil { // warm the cache
		t.Fatal(err)
	}
	got, err := s.SampleProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := clock.get()
	if err != nil {
		t.Fatal(err)
	}
	want, err := referenceSample("/proc", boot)
	if err != nil {
		t.Fatal(err)
	}
	type identity struct {
		key  domain.ProcessKey
		ppid int
		uid  uint32
		name string
	}
	ids := func(ps []domain.ProcessSample) map[int]identity {
		m := map[int]identity{}
		for _, p := range ps {
			m[p.PID] = identity{key: p.Key(), ppid: p.PPID, uid: p.UID, name: p.Name}
		}
		return m
	}
	g, w := ids(got), ids(want)
	matched := 0
	for pid, wi := range w {
		gi, ok := g[pid]
		if !ok || gi.key != wi.key {
			continue // exited or reused between the reads
		}
		if gi != wi {
			t.Errorf("pid %d: got %+v, reference %+v", pid, gi, wi)
		}
		matched++
	}
	if matched < 2 {
		t.Fatalf("only %d processes in both samples", matched)
	}
}
