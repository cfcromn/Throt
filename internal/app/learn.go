package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/process"
	"github.com/cfcromn/Throt/internal/signature"
)

// Learned heavy commands. The Claude Code hook and `throt run` report the
// signature (internal/signature) of each command an agent is about to
// start; throtd never sees more of the command. When a heavy workload of
// that agent then starts within learnWindow, and no other reported command
// of the agent could have started it, the signature is learned heavy, so
// the hook waits for capacity before it the next time, as for a build tool
// known by name. A learned signature that runs learnMisses times in a row
// without a heavy workload is forgotten. Learned signatures persist in
// learned.json in the private socket directory.
const (
	// learnWindow is how long after a report a workload may start and
	// still be the reported command's.
	learnWindow = 10 * time.Second
	// learnSlack allows for start times truncated to the second.
	learnSlack = 2 * time.Second
	// pendingTTL is how long a report waits for its command to become a
	// heavy workload; after it, the run counts as light.
	pendingTTL = 2 * time.Minute
	// learnMisses light runs in a row forget a learned signature.
	learnMisses = 3
	// maxLearned bounds the learned signatures; the one heavy longest ago
	// is evicted first.
	maxLearned = 256
	// maxPending bounds the reports waiting for a workload.
	maxPending = 256
	// learnedVersion is the version of learned.json.
	learnedVersion = 1
	// maxLearnedFile bounds what is read of learned.json.
	maxLearnedFile = 1 << 20
	// peakGrowth is how much a tracked workload's memory must grow over the
	// stored peak before the file is rewritten.
	peakGrowth = 1.25
)

// LearnedSignature is one signature learned heavy.
type LearnedSignature struct {
	Signature string `json:"signature"`
	// PeakRSS is the largest resident memory, in bytes, seen of a workload
	// it started.
	PeakRSS uint64 `json:"peak_rss"`
	// HeavyRuns counts the runs that became heavy workloads.
	HeavyRuns int `json:"heavy_runs"`
	// Misses counts the light runs since the last heavy one.
	Misses int `json:"misses"`
	// LastHeavy is when a run last became a heavy workload.
	LastHeavy time.Time `json:"last_heavy"`
}

// pendingRun is a reported command that has not become a workload yet.
type pendingRun struct {
	agent int // the agent's PID
	sig   string
	named bool // heavy by name: never learned, but it makes others ambiguous
	// A workload of the run starts between since-learnSlack and
	// at+learnWindow: since is the reporter's own start, at its latest
	// report.
	since, at time.Time
}

// learner holds the learned signatures and the reports waiting for a
// workload. It is not safe for concurrent use; the daemon guards it with
// learnMu.
type learner struct {
	learned map[string]*LearnedSignature
	pending []pendingRun
	// seen holds the heavy workloads already matched against reports;
	// tracked maps those that took a learned signature to it, to follow
	// their peak memory. Both are pruned to the current heavy workloads.
	seen    map[domain.ProcessKey]bool
	tracked map[domain.ProcessKey]string
	// dirty is set when learned changed since it was last saved.
	dirty bool
}

func newLearner() *learner {
	return &learner{learned: map[string]*LearnedSignature{}, seen: map[domain.ProcessKey]bool{}, tracked: map[domain.ProcessKey]string{}}
}

// expect records that agent (0 when unattributed) is about to start sig,
// reported by a process that started at since, and reports whether sig is
// learned heavy.
func (l *learner) expect(agent int, sig string, named bool, since, now time.Time) bool {
	_, heavy := l.learned[sig]
	if agent == 0 {
		return heavy
	}
	if i := slices.IndexFunc(l.pending, func(p pendingRun) bool { return p.agent == agent && p.sig == sig }); i >= 0 {
		p := &l.pending[i]
		p.at, p.named = now, p.named || named
		if since.Before(p.since) {
			p.since = since
		}
		return heavy
	}
	if len(l.pending) == maxPending {
		l.pending = l.pending[1:]
	}
	if since.IsZero() || since.After(now) {
		since = now
	}
	l.pending = append(l.pending, pendingRun{agent: agent, sig: sig, named: named, since: since, at: now})
	return heavy
}

// observe matches the heavy workloads of a fresh snapshot against the
// reports and ages the reports out. It returns the signatures learned now.
func (l *learner) observe(now time.Time, heavy []process.WorkloadView) []string {
	var learned []string
	current := make(map[domain.ProcessKey]bool, len(heavy))
	for _, w := range heavy {
		current[w.Key] = true
		if sig, ok := l.tracked[w.Key]; ok {
			l.grow(sig, w.RSS)
		}
		if l.seen[w.Key] {
			continue
		}
		l.seen[w.Key] = true
		if w.OwnerPID == 0 {
			continue
		}
		var match []int
		for i, p := range l.pending {
			if p.agent == w.OwnerPID && !w.Key.Start.Before(p.since.Add(-learnSlack)) && !w.Key.Start.After(p.at.Add(learnWindow)) {
				match = append(match, i)
			}
		}
		// Two reports could each have started it: learn nothing from it,
		// and count neither run as light.
		if len(match) == 1 && !l.pending[match[0]].named {
			sig := l.pending[match[0]].sig
			l.heavyRun(sig, w.RSS, now)
			l.tracked[w.Key] = sig
			learned = append(learned, sig)
		}
		for _, i := range slices.Backward(match) {
			l.pending = slices.Delete(l.pending, i, i+1)
		}
	}
	for k := range l.seen {
		if !current[k] {
			delete(l.seen, k)
			delete(l.tracked, k)
		}
	}
	l.pending = slices.DeleteFunc(l.pending, func(p pendingRun) bool {
		if now.Sub(p.at) < pendingTTL {
			return false
		}
		if e, ok := l.learned[p.sig]; ok && !p.named {
			l.dirty = true
			if e.Misses++; e.Misses >= learnMisses {
				delete(l.learned, p.sig)
			}
		}
		return true
	})
	return learned
}

// heavyRun records a run of sig that became a heavy workload using rss.
func (l *learner) heavyRun(sig string, rss uint64, now time.Time) {
	e, ok := l.learned[sig]
	if !ok {
		if len(l.learned) >= maxLearned {
			l.evict()
		}
		e = &LearnedSignature{Signature: sig}
		l.learned[sig] = e
	}
	e.HeavyRuns++
	e.Misses = 0
	e.LastHeavy = now
	e.PeakRSS = max(e.PeakRSS, rss)
	l.dirty = true
}

// grow raises sig's peak to rss; it marks the state dirty only when the
// peak grew by peakGrowth, so a growing workload rewrites the file a few
// times, not every sample.
func (l *learner) grow(sig string, rss uint64) {
	e, ok := l.learned[sig]
	if !ok || rss <= e.PeakRSS {
		return
	}
	if float64(rss) >= float64(e.PeakRSS)*peakGrowth {
		l.dirty = true
	}
	e.PeakRSS = rss
}

// evict drops the learned signature heavy longest ago.
func (l *learner) evict() {
	var oldest *LearnedSignature
	for _, e := range l.learned {
		if oldest == nil || e.LastHeavy.Before(oldest.LastHeavy) || e.LastHeavy.Equal(oldest.LastHeavy) && e.Signature < oldest.Signature {
			oldest = e
		}
	}
	if oldest != nil {
		delete(l.learned, oldest.Signature)
	}
}

// list returns the learned signatures, most recently heavy first.
func (l *learner) list() []LearnedSignature {
	out := make([]LearnedSignature, 0, len(l.learned))
	for _, e := range l.learned {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b LearnedSignature) int {
		return cmp.Or(b.LastHeavy.Compare(a.LastHeavy), cmp.Compare(a.Signature, b.Signature))
	})
	return out
}

// forget drops sig, or every signature when sig is "", and returns how
// many it dropped.
func (l *learner) forget(sig string) int {
	n := len(l.learned)
	if sig == "" {
		clear(l.learned)
		clear(l.tracked)
	} else {
		delete(l.learned, sig)
	}
	n -= len(l.learned)
	if n > 0 {
		l.dirty = true
	}
	return n
}

// learnedFile is the format of learned.json.
type learnedFile struct {
	Version    int                `json:"version"`
	Signatures []LearnedSignature `json:"signatures"`
}

// marshal encodes the learned signatures for learned.json.
func (l *learner) marshal() ([]byte, error) {
	return json.Marshal(learnedFile{Version: learnedVersion, Signatures: l.list()})
}

// errLearnedFile reports a learned.json that is not version 1 JSON.
var errLearnedFile = errors.New("malformed learned signatures file")

// load replaces the learned signatures with those in data. Entries that
// are not valid are skipped; beyond maxLearned the most recently heavy are
// kept. On error nothing is loaded and the state is marked dirty, so the
// file is replaced at the next save.
func (l *learner) load(data []byte) error {
	var f learnedFile
	if len(data) > maxLearnedFile || json.Unmarshal(data, &f) != nil || f.Version != learnedVersion {
		l.dirty = true
		return errLearnedFile
	}
	clear(l.learned)
	slices.SortStableFunc(f.Signatures, func(a, b LearnedSignature) int { return b.LastHeavy.Compare(a.LastHeavy) })
	for _, e := range f.Signatures {
		if len(l.learned) == maxLearned {
			l.dirty = true
			break
		}
		if !signature.Valid(e.Signature) || l.learned[e.Signature] != nil || e.HeavyRuns < 1 || e.Misses < 0 || e.Misses >= learnMisses {
			l.dirty = true
			continue
		}
		l.learned[e.Signature] = &e
	}
	return nil
}

// ExpectParams name the caller and the command it is about to start, for
// the "expect" method.
type ExpectParams struct {
	// PID is the caller's own PID, checked and resolved as for acquire.
	PID int `json:"pid"`
	// Signature is the command's signature (internal/signature).
	Signature string `json:"signature"`
	// Named says the client already treats the command as heavy by name:
	// it is never learned, but makes another report of the agent ambiguous.
	Named bool `json:"named,omitempty"`
}

// ExpectResult is returned by the "expect" method.
type ExpectResult struct {
	// Heavy reports that the signature is learned heavy.
	Heavy bool `json:"heavy"`
}

// LearnedParams select what the "learned" method forgets before it lists.
type LearnedParams struct {
	// Forget drops one signature.
	Forget string `json:"forget,omitempty"`
	// Clear drops every signature.
	Clear bool `json:"clear,omitempty"`
}

// LearnedResult is returned by the "learned" method.
type LearnedResult struct {
	// Signatures are the learned signatures, most recently heavy first.
	Signatures []LearnedSignature `json:"signatures"`
	// Forgotten counts the signatures this call dropped.
	Forgotten int `json:"forgotten"`
}

// expect handles the "expect" method: the caller's agent is about to start
// the command with the given signature. The answer says whether it is
// learned heavy; the report lets throtd learn it.
func (d *Daemon) expect(ctx context.Context, raw json.RawMessage) (any, error) {
	var p ExpectParams
	if err := decodeParams(raw, &p); err != nil {
		return nil, err
	}
	if err := checkPID(p.PID); err != nil {
		return nil, err
	}
	if !signature.Valid(p.Signature) {
		return nil, errors.New("invalid params: signature is not a command signature")
	}
	self, code, why := d.caller(ctx, p.PID)
	if code != "" {
		return nil, fmt.Errorf("expect refused: %s", why)
	}
	chain := d.ancestry(ctx, self.PPID)
	d.mu.Lock()
	a, _ := nearestAgentView(chain, d.status.Agents)
	d.mu.Unlock()
	d.learnMu.Lock()
	heavy := d.learn.expect(a.Key.PID, p.Signature, p.Named, self.Start, time.Now())
	d.learnMu.Unlock()
	return ExpectResult{Heavy: heavy}, nil
}

// learnedSignatures handles the "learned" method.
func (d *Daemon) learnedSignatures(_ context.Context, raw json.RawMessage) (any, error) {
	var p LearnedParams
	if len(raw) > 0 {
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}
	}
	var r LearnedResult
	d.learnMu.Lock()
	switch {
	case p.Clear:
		r.Forgotten = d.learn.forget("")
	case p.Forget != "":
		r.Forgotten = d.learn.forget(p.Forget)
	}
	r.Signatures = d.learn.list()
	d.learnMu.Unlock()
	if r.Forgotten > 0 {
		d.saveLearned()
	}
	return r, nil
}

// learnFrom feeds a fresh snapshot's heavy workloads to the learner and
// saves what changed.
func (d *Daemon) learnFrom(now time.Time, heavy []process.WorkloadView) {
	d.learnMu.Lock()
	learned := d.learn.observe(now, heavy)
	dirty := d.learn.dirty
	d.learnMu.Unlock()
	// Signatures are command text: never logged at info level.
	for _, sig := range learned {
		d.log.Debug("learned a heavy command", "signature", sig)
	}
	if dirty {
		d.saveLearned()
	}
}

// learnedPath holds the learned signatures, next to paused.json in the
// private socket directory.
func (d *Daemon) learnedPath() string {
	return filepath.Join(filepath.Dir(d.cfg.SocketPath), "learned.json")
}

// loadLearned reads learned.json. A missing file starts empty; one that
// cannot be read or parsed is ignored, and replaced at the next save.
func (d *Daemon) loadLearned() {
	b, err := readLimited(d.learnedPath(), maxLearnedFile)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	d.learnMu.Lock()
	defer d.learnMu.Unlock()
	if err == nil {
		err = d.learn.load(b)
	}
	if err != nil {
		d.learn.dirty = true
		d.log.Warn("ignoring the learned heavy commands; the file will be replaced", "path", d.learnedPath(), "err", err)
	}
}

// saveLearned writes learned.json if the learned signatures changed.
// learnSaveMu orders the writes, so an older state never replaces a newer
// one; learnMu is not held while writing.
func (d *Daemon) saveLearned() {
	d.learnSaveMu.Lock()
	defer d.learnSaveMu.Unlock()
	d.learnMu.Lock()
	if !d.learn.dirty {
		d.learnMu.Unlock()
		return
	}
	data, err := d.learn.marshal()
	d.learn.dirty = false
	d.learnMu.Unlock()
	if err == nil {
		err = d.writeFile(d.learnedPath(), data)
	}
	if err != nil {
		d.log.Warn("save learned heavy commands", "err", err)
		d.learnMu.Lock()
		d.learn.dirty = true
		d.learnMu.Unlock()
	}
}

// readLimited reads path, refusing a file larger than limit.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("larger than %d bytes", limit)
	}
	return b, err
}
