package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// toolsWant pins tools/list, the stable MCP surface: each tool's name,
// input schema and output schema, in order, as compact JSON with sorted
// keys. Changing it is a compatibility decision: removing or renaming a
// tool, an input or an output field, or changing a type, is breaking (see
// the Compatibility section of docs/ARCHITECTURE.md); adding an optional
// input or an output field is not. Tool descriptions are not pinned.
var toolsWant = []toolWant{
	{"throt_status",
		`{"additionalProperties":false,"properties":{},"type":"object"}`,
		`{"properties":{"agents":{"items":{"properties":{"agent":{"type":"string"},"children":{"type":"integer"},"cpu_percent":{"type":"integer"},"memory":{"type":"string"},"pid":{"type":"integer"}},"required":["agent","pid","cpu_percent","memory","children"],"type":"object"},"type":"array"},"degraded":{"type":"string"},"heavy":{"items":{"properties":{"class":{"type":"string"},"cpu_percent":{"type":"integer"},"memory":{"type":"string"},"name":{"type":"string"},"owner":{"type":"string"},"pid":{"type":"integer"},"state":{"type":"string"}},"required":["owner","pid","name","class","state","cpu_percent","memory"],"type":"object"},"type":"array"},"host":{"properties":{"cpu":{"type":"string"},"latency":{"type":"string"},"load":{"type":"string"},"memory":{"type":"string"},"swap":{"type":"string"}},"required":["cpu","load","memory","swap","latency"],"type":["object","null"]},"pressure":{"type":"string"},"pressure_reason":{"type":"string"},"scheduler":{"properties":{"heavy_limit":{"type":"integer"},"mode":{"type":"string"},"queued":{"type":"integer"}},"required":["mode","heavy_limit","queued"],"type":"object"}},"required":["pressure","host","agents","heavy","scheduler"],"type":"object"}`},
	{"throt_capacity",
		`{"additionalProperties":false,"properties":{"agent":{"description":"Optional agent instance label, e.g. \"claude-1\", to report that agent's running heavy workloads.","maxLength":128,"minLength":1,"type":"string"}},"type":"object"}`,
		`{"properties":{"agent":{"type":"string"},"agent_running_heavy":{"type":["integer","null"]},"heavy_limit":{"type":"integer"},"ok_to_start":{"type":"boolean"},"pressure":{"type":"string"},"pressure_reason":{"type":"string"},"reason":{"type":"string"},"reserved":{"type":"integer"},"running_heavy":{"type":"integer"},"scheduler_mode":{"type":"string"},"suggestion":{"type":"string"}},"required":["ok_to_start","reason","pressure","scheduler_mode","running_heavy","reserved","heavy_limit","suggestion"],"type":"object"}`},
	{"throt_wait_for_capacity",
		`{"additionalProperties":false,"properties":{"agent":{"description":"Optional agent instance label, e.g. \"claude-1\"; defaults to the agent that launched this server.","maxLength":128,"minLength":1,"type":"string"},"max_wait_seconds":{"default":60,"description":"Longest time to wait, in seconds.","maximum":600,"minimum":1,"type":"integer"}},"type":"object"}`,
		`{"properties":{"agent":{"type":"string"},"agent_running_heavy":{"type":["integer","null"]},"checks":{"type":"integer"},"heavy_limit":{"type":"integer"},"ok_to_start":{"type":"boolean"},"pressure":{"type":"string"},"pressure_reason":{"type":"string"},"reason":{"type":"string"},"reserved":{"type":"integer"},"running_heavy":{"type":"integer"},"scheduler_mode":{"type":"string"},"suggestion":{"type":"string"},"timed_out":{"type":"boolean"},"waited_seconds":{"type":"number"}},"required":["ok_to_start","reason","pressure","scheduler_mode","running_heavy","reserved","heavy_limit","suggestion","waited_seconds","checks","timed_out"],"type":"object"}`},
	{"throt_explain",
		`{"additionalProperties":false,"properties":{"pid":{"description":"PID of a heavy workload, as listed by throt_status.","maximum":2147483647,"minimum":1,"type":"integer"}},"required":["pid"],"type":"object"}`,
		`{"properties":{"class":{"type":"string"},"events":{"items":{"properties":{"action":{"type":"string"},"kind":{"type":"string"},"pressure":{"type":"string"},"reason":{"type":"string"},"slots":{"type":"string"},"time":{"type":"string"}},"required":["time","kind","action","reason","pressure","slots"],"type":"object"},"type":"array"},"name":{"type":"string"},"owner":{"type":"string"},"owner_pid":{"type":"integer"},"pid":{"type":"integer"},"pressure":{"type":"string"},"scheduler":{"properties":{"heavy_limit":{"type":"integer"},"mode":{"type":"string"},"per_agent_limit":{"type":"integer"},"running":{"type":"integer"}},"required":["mode","heavy_limit","running"],"type":"object"},"state":{"type":"string"},"summary":{"type":"string"}},"required":["pid","name","class","state","scheduler","pressure","events","summary"],"type":"object"}`},
	{"throt_events",
		`{"additionalProperties":false,"properties":{"limit":{"default":20,"description":"How many of the newest events to return.","maximum":200,"minimum":1,"type":"integer"}},"type":"object"}`,
		`{"properties":{"events":{"items":{"properties":{"attrs":{"additionalProperties":{"type":"string"},"type":"object"},"instance":{"type":"string"},"kind":{"type":"string"},"message":{"type":"string"},"seq":{"type":"integer"},"time":{"format":"date-time","type":"string"}},"required":["seq","time","kind"],"type":"object"},"type":"array"}},"required":["events"],"type":"object"}`},
	{"throt_stats",
		`{"additionalProperties":false,"properties":{},"type":"object"}`,
		`{"properties":{"agents":{"items":{"properties":{"active_seconds":{"type":"number"},"agent":{"type":"string"},"cpu_seconds":{"type":"number"},"heavy_jobs":{"type":"integer"},"peak_memory":{"type":"string"},"pid":{"type":"integer"},"queued_seconds":{"type":"number"}},"required":["agent","cpu_seconds","peak_memory","heavy_jobs","queued_seconds","active_seconds"],"type":"object"},"type":"array"},"since":{"type":"string"},"total":{"properties":{"active_seconds":{"type":"number"},"agent":{"type":"string"},"cpu_seconds":{"type":"number"},"heavy_jobs":{"type":"integer"},"peak_memory":{"type":"string"},"pid":{"type":"integer"},"queued_seconds":{"type":"number"}},"required":["agent","cpu_seconds","peak_memory","heavy_jobs","queued_seconds","active_seconds"],"type":"object"},"unattributed":{"properties":{"active_seconds":{"type":"number"},"agent":{"type":"string"},"cpu_seconds":{"type":"number"},"heavy_jobs":{"type":"integer"},"peak_memory":{"type":"string"},"pid":{"type":"integer"},"queued_seconds":{"type":"number"}},"required":["agent","cpu_seconds","peak_memory","heavy_jobs","queued_seconds","active_seconds"],"type":"object"}},"required":["agents","total","unattributed"],"type":"object"}`},
}

type toolWant struct{ name, input, output string }

func TestToolsListSnapshot(t *testing.T) {
	m := one(t, &fakeClient{}, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	var r struct {
		Tools []struct {
			Name         string
			InputSchema  json.RawMessage
			OutputSchema json.RawMessage
		}
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	var got []toolWant
	for _, tl := range r.Tools {
		got = append(got, toolWant{tl.Name, compact(t, tl.InputSchema), compact(t, tl.OutputSchema)})
	}
	if !slices.Equal(got, toolsWant) {
		var b strings.Builder
		for _, w := range got {
			fmt.Fprintf(&b, "\t{%q,\n\t\t`%s`,\n\t\t`%s`},\n", w.name, w.input, w.output)
		}
		t.Errorf("tools/list changed; if on purpose, update toolsWant to:\n%s", b.String())
	}
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	// Round-trip so map keys are sorted whatever the source order.
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("schema %s: %v", raw, err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// checkOutput fails t unless structured, the structuredContent of a
// successful call of the named tool, conforms to the tool's outputSchema. It is
// stricter than the schema: a property the schema does not list fails too,
// so the schema cannot fall behind what the tools return.
func checkOutput(t *testing.T, name string, structured json.RawMessage) {
	t.Helper()
	i := slices.IndexFunc(toolList(), func(tl tool) bool { return tl.Name == name })
	if i < 0 {
		t.Fatalf("no tool %s", name)
	}
	var schema, v any
	if err := json.Unmarshal(toolList()[i].OutputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if len(structured) == 0 {
		t.Fatalf("%s: no structuredContent", name)
	}
	if err := json.Unmarshal(structured, &v); err != nil {
		t.Fatal(err)
	}
	for _, err := range validate(schema.(map[string]any), v, "$") {
		t.Errorf("%s: %v\nstructuredContent: %s", name, err, structured)
	}
}

// validate checks v against the subset of JSON Schema that outputSchema
// produces: type (a name or a list of names), properties, required, items
// and additionalProperties.
func validate(s map[string]any, v any, path string) []error {
	var types []string
	switch ty := s["type"].(type) {
	case string:
		types = []string{ty}
	case []any:
		for _, x := range ty {
			types = append(types, x.(string))
		}
	}
	if !slices.Contains(types, jsonType(v)) && !(jsonType(v) == "integer" && slices.Contains(types, "number")) {
		return []error{fmt.Errorf("%s: %s, want %v", path, jsonType(v), types)}
	}
	var errs []error
	switch v := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		extra, _ := s["additionalProperties"].(map[string]any)
		req, _ := s["required"].([]any)
		for _, r := range req {
			if _, ok := v[r.(string)]; !ok {
				errs = append(errs, fmt.Errorf("%s: missing required %q", path, r))
			}
		}
		for k, x := range v {
			switch {
			case props[k] != nil:
				errs = append(errs, validate(props[k].(map[string]any), x, path+"."+k)...)
			case extra != nil:
				errs = append(errs, validate(extra, x, path+"."+k)...)
			default:
				errs = append(errs, fmt.Errorf("%s: %q is not in the schema", path, k))
			}
		}
	case []any:
		for i, x := range v {
			errs = append(errs, validate(s["items"].(map[string]any), x, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return errs
}

func jsonType(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if v == float64(int64(v)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func TestValidate(t *testing.T) {
	schema := map[string]any{}
	if err := json.Unmarshal(outputSchema(waitView{}), &schema); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		doc  string
		want string // substring of the first error; "" for valid
	}{
		{`{"ok_to_start":true,"reason":"","pressure":"normal","scheduler_mode":"off","running_heavy":0,"reserved":0,"heavy_limit":0,"suggestion":"","waited_seconds":0.5,"checks":1,"timed_out":false}`, ""},
		{`{"ok_to_start":true,"reason":"","pressure":"normal","scheduler_mode":"off","running_heavy":0,"reserved":0,"heavy_limit":0,"suggestion":"","waited_seconds":0,"checks":1,"timed_out":false,"agent_running_heavy":null}`, ""},
		{`{"ok_to_start":true,"reason":"","pressure":"normal","scheduler_mode":"off","running_heavy":0,"reserved":0,"heavy_limit":0,"suggestion":"","waited_seconds":0,"checks":1}`, `missing required "timed_out"`},
		{`{"ok_to_start":true,"reason":"","pressure":"normal","scheduler_mode":"off","running_heavy":0.5,"reserved":0,"heavy_limit":0,"suggestion":"","waited_seconds":0,"checks":1,"timed_out":false}`, "$.running_heavy: number"},
		{`{"ok_to_start":true,"reason":"","pressure":"normal","scheduler_mode":"off","running_heavy":0,"reserved":0,"heavy_limit":0,"suggestion":"","waited_seconds":0,"checks":1,"timed_out":false,"new":1}`, `"new" is not in the schema`},
		{`[]`, "$: array"},
	} {
		var v any
		if err := json.Unmarshal([]byte(tc.doc), &v); err != nil {
			t.Fatal(err)
		}
		errs := validate(schema, v, "$")
		switch {
		case tc.want == "" && len(errs) > 0:
			t.Errorf("%s: %v", tc.doc, errs)
		case tc.want != "" && (len(errs) == 0 || !strings.Contains(errs[0].Error(), tc.want)):
			t.Errorf("%s: errors %v, want %q", tc.doc, errs, tc.want)
		}
	}
}
