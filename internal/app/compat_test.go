package app

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// The compatibility tests pin the stable surface of local protocol v1 and
// the event log (docs/ARCHITECTURE.md, Compatibility). Each expectation is
// inline: changing one is a compatibility decision. Removing or renaming a
// method, param or result field breaks v1 clients; adding one is allowed
// and must be added here on purpose.

// protocolWant lists, per method, the JSON field paths of its params and of
// its result: "a.b" is field b of object a, "a[]" an element of array a,
// and "attrs.*" any key of a string map.
var protocolWant = map[string]struct {
	params, result []string
}{
	"acquire": {
		params: []string{
			"overdue",
			"pid",
		},
		result: []string{
			"granted", "lease_id", "reason", "verdict.agent", "verdict.agent_running_heavy",
			"verdict.heavy_limit", "verdict.ok_to_start", "verdict.pressure", "verdict.pressure_reason",
			"verdict.reason", "verdict.reserved", "verdict.running_heavy", "verdict.scheduler_mode",
			"verdict.suggestion",
		},
	},
	"expect": {
		params: []string{
			"named", "pid", "signature",
		},
		result: []string{
			"heavy",
		},
	},
	"learned": {
		params: []string{
			"clear", "forget",
		},
		result: []string{
			"forgotten", "signatures[].heavy_runs", "signatures[].last_heavy", "signatures[].misses",
			"signatures[].peak_rss", "signatures[].signature",
		},
	},
	"events": {
		params: []string{
			"limit", "since",
		},
		result: []string{
			"[].attrs.*", "[].instance", "[].kind", "[].message", "[].seq", "[].time",
		},
	},
	"pause": {
		params: []string{
			"force", "pid", "start",
		},
		result: nil,
	},
	"ping": {
		params: nil,
		result: []string{
			"pid", "protocol", "uptime", "version",
		},
	},
	"reload": {
		params: nil,
		result: []string{
			"changed[]", "requires_restart[]",
		},
	},
	"resume": {
		params: []string{
			"force", "pid", "start",
		},
		result: nil,
	},
	"stats": {
		params: nil,
		result: []string{
			"agents[].agent", "agents[].cpu_seconds", "agents[].first_seen", "agents[].heavy_started",
			"agents[].last_seen", "agents[].peak_rss", "agents[].process.pid", "agents[].process.start",
			"agents[].queued_seconds", "since", "total.agent", "total.cpu_seconds", "total.first_seen",
			"total.heavy_started", "total.last_seen", "total.peak_rss", "total.process.pid",
			"total.process.start", "total.queued_seconds", "unattributed.agent",
			"unattributed.cpu_seconds", "unattributed.first_seen", "unattributed.heavy_started",
			"unattributed.last_seen", "unattributed.peak_rss", "unattributed.process.pid",
			"unattributed.process.start", "unattributed.queued_seconds",
		},
	},
	"status": {
		params: nil,
		result: []string{
			"agents[].agent", "agents[].children", "agents[].cpu", "agents[].name", "agents[].process.pid",
			"agents[].process.start", "agents[].rss", "degraded", "heavy[].background", "heavy[].class", "heavy[].cpu",
			"heavy[].idle", "heavy[].name", "heavy[].owner", "heavy[].owner_pid", "heavy[].process.pid",
			"heavy[].process.start", "heavy[].rss", "host.cpu_stall.full", "host.cpu_stall.some",
			"host.cpu_usage", "host.io_stall.full", "host.io_stall.some", "host.latency", "host.load1",
			"host.mem_available", "host.mem_total", "host.memory_pressure", "host.memory_stall.full",
			"host.memory_stall.some", "host.missing[]", "host.swap_total", "host.swap_used", "host.time",
			"orphan_count", "orphans[].class", "orphans[].hint", "orphans[].kind", "orphans[].name",
			"orphans[].owner", "orphans[].process.pid", "orphans[].process.start", "orphans[].since",
			"paused[].by", "paused[].members[].pid", "paused[].members[].start", "paused[].name",
			"paused[].owner", "paused[].process.pid", "paused[].process.start", "paused[].since",
			"paused[].state", "pressure", "reason", "scheduler.adaptive", "scheduler.heavy_limit",
			"scheduler.leases[].agent", "scheduler.leases[].granted", "scheduler.leases[].id",
			"scheduler.leases[].process.pid", "scheduler.leases[].process.start", "scheduler.max_limit",
			"scheduler.mode", "scheduler.per_agent_limit", "scheduler.queued[].pid",
			"scheduler.queued[].start", "scheduler.start_limit", "scheduler.would_queue[].pid",
			"scheduler.would_queue[].start", "unattributed_memory[].name", "unattributed_memory[].processes",
			"unattributed_memory[].rss", "unattributed_rss",
		},
	},
}

// protocolTypes maps each method to its params and result types; nil means
// none (the result is JSON null).
var protocolTypes = map[string]struct{ params, result any }{
	"ping":    {nil, PingResult{}},
	"status":  {nil, StatusResult{}},
	"stats":   {nil, StatsResult{}},
	"events":  {EventsParams{}, []domain.Event{}},
	"pause":   {ControlParams{}, nil},
	"resume":  {ControlParams{}, nil},
	"reload":  {nil, ReloadResult{}},
	"acquire": {AcquireParams{}, AcquireResult{}},
	"expect":  {ExpectParams{}, ExpectResult{}},
	"learned": {LearnedParams{}, LearnedResult{}},
}

func TestProtocolV1Methods(t *testing.T) {
	d := &Daemon{}
	methods := slices.Sorted(maps.Keys(d.handlers()))
	if want := slices.Sorted(maps.Keys(protocolWant)); !slices.Equal(methods, want) {
		t.Errorf("methods = %v, want %v", methods, want)
	}
	if want := slices.Sorted(maps.Keys(protocolTypes)); !slices.Equal(methods, want) {
		t.Errorf("methods = %v, protocolTypes has %v", methods, want)
	}
	if got, want := privilegedMethods(), []string{"pause", "resume", "reload", "acquire", "expect", "learned"}; !slices.Equal(got, want) {
		t.Errorf("privileged = %v, want %v", got, want)
	}
}

// TestProtocolV1Fields encodes a representative params and result value
// of every method, with every field set, and compares the JSON field paths
// with protocolWant, so a renamed, removed or added field fails.
func TestProtocolV1Fields(t *testing.T) {
	var dump strings.Builder
	for _, m := range slices.Sorted(maps.Keys(protocolTypes)) {
		types := protocolTypes[m]
		params, result := jsonPaths(t, types.params), jsonPaths(t, types.result)
		want := protocolWant[m]
		if !slices.Equal(params, want.params) || !slices.Equal(result, want.result) {
			t.Errorf("%s: params %q, result %q; want params %q, result %q", m, params, result, want.params, want.result)
		}
		fmt.Fprintf(&dump, "\t%q: {%s, %s},\n", m, goStrings(params), goStrings(result))
	}
	if t.Failed() {
		t.Logf("if the change is intended and compatible, protocolWant is:\n%s", dump.String())
	}
}

func goStrings(ss []string) string {
	if ss == nil {
		return "nil"
	}
	return fmt.Sprintf("%#v", ss)
}

// jsonPaths returns the sorted JSON field paths of a value of v's type
// with every field set (see fill), or nil for a nil v.
func jsonPaths(t *testing.T, v any) []string {
	t.Helper()
	if v == nil {
		return nil
	}
	rv := reflect.New(reflect.TypeOf(v)).Elem()
	fill(rv)
	b, err := json.Marshal(rv.Interface())
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var paths []string
	var walk func(prefix string, x any)
	walk = func(prefix string, x any) {
		switch x := x.(type) {
		case map[string]any:
			for k, y := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, y)
			}
		case []any:
			walk(prefix+"[]", x[0])
		default:
			paths = append(paths, prefix)
		}
	}
	walk("", doc)
	slices.Sort(paths)
	return paths
}

// fill sets every field reachable from v to a non-zero value, so no
// omitempty field is left out: one element per slice, one "*" key per map.
func fill(v reflect.Value) {
	if v.Type() == reflect.TypeFor[time.Time]() {
		v.Set(reflect.ValueOf(time.Unix(1, 0).UTC()))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Field(i); f.CanSet() {
				fill(f)
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		k.SetString("*")
		fill(e)
		v.SetMapIndex(k, e)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	default:
		panic("fill: " + v.Type().String())
	}
}

// docCodes returns the codes in the first column of the table in
// docs/ARCHITECTURE.md whose header row starts with header: every
// backticked word in that cell.
func docCodes(t *testing.T, header string) []string {
	t.Helper()
	b, err := os.ReadFile("../../docs/ARCHITECTURE.md")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, header) })
	if i < 0 {
		t.Fatalf("docs/ARCHITECTURE.md has no table %q", header)
	}
	var codes []string
	for _, l := range lines[i+2:] {
		if !strings.HasPrefix(l, "|") {
			break
		}
		cell := strings.SplitN(l, "|", 3)[1]
		for j, part := range strings.Split(cell, "`") {
			if j%2 == 1 {
				codes = append(codes, part)
			}
		}
	}
	return codes
}

// sameSet fails t unless the documented codes equal the code's.
func sameSet[S ~string](t *testing.T, what string, doc []string, code []S) {
	t.Helper()
	var c []string
	for _, s := range code {
		c = append(c, string(s))
	}
	slices.Sort(c)
	c = slices.Compact(c)
	doc = slices.Clone(doc)
	slices.Sort(doc)
	if !slices.Equal(doc, c) {
		t.Errorf("%s: docs/ARCHITECTURE.md lists %v, the code has %v", what, doc, c)
	}
}

func TestDocumentedCodes(t *testing.T) {
	sameSet(t, "event kinds", docCodes(t, "| Event kind |"), domain.EventKinds)
	sameSet(t, "decision reasons", docCodes(t, "| Decision reason |"), DecisionReasons)
	sameSet(t, "limit reasons", docCodes(t, "| Limit reason |"), scheduler.LimitReasons)
	sameSet(t, "refusal reasons", docCodes(t, "| Refusal reason |"), RefusalReasons)
	sameSet(t, "protocol methods", docCodes(t, "| Method |"), slices.Collect(maps.Keys((&Daemon{}).handlers())))
}
