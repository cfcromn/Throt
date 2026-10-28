package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/domain"
	"github.com/cfcromn/Throt/internal/scheduler"
)

// modernMeta is the _meta a 2026-07-28 client sends on every request.
const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}`

// modernResultFields checks the members every modern result carries.
func modernResultFields(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var r map[string]json.RawMessage
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if string(r["resultType"]) != `"complete"` {
		t.Errorf("resultType = %s", r["resultType"])
	}
	var meta map[string]struct{ Name, Version string }
	if err := json.Unmarshal(r["_meta"], &meta); err != nil {
		t.Fatalf("_meta = %s: %v", r["_meta"], err)
	}
	if si := meta[metaServerInfo]; si.Name != "throt" || si.Version != "1.2.3" {
		t.Errorf("serverInfo = %+v", si)
	}
	return r
}

func TestServerDiscover(t *testing.T) {
	m := one(t, &fakeClient{}, `{"jsonrpc":"2.0","id":"discover-1","method":"server/discover","params":{`+modernMeta+`}}`+"\n")
	if m.Error != nil || string(m.ID) != `"discover-1"` {
		t.Fatalf("id %s err %+v", m.ID, m.Error)
	}
	r := modernResultFields(t, m.Result)
	var versions []string
	if err := json.Unmarshal(r["supportedVersions"], &versions); err != nil {
		t.Fatal(err)
	}
	if strings.Join(versions, ",") != "2026-07-28,2025-11-25,2025-06-18,2024-11-05" {
		t.Errorf("supportedVersions = %v", versions)
	}
	if string(r["capabilities"]) != `{"tools":{}}` || string(r["ttlMs"]) != "3600000" || string(r["cacheScope"]) != `"public"` {
		t.Errorf("result = %s", m.Result)
	}
	if !strings.Contains(string(r["instructions"]), "throt_wait_for_capacity") {
		t.Errorf("instructions = %s", r["instructions"])
	}
}

// TestEachSupportedVersion opens a stream the way a client of each
// revision does and lists the tools.
func TestEachSupportedVersion(t *testing.T) {
	for _, v := range supportedVersions() {
		t.Run(v, func(t *testing.T) {
			var in string
			list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
			if v == modernVersion {
				in = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + modernMeta + `}}` + "\n"
				list = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + modernMeta + `}}`
			} else {
				in = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + v + `"}}` + "\n" +
					`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
			}
			msgs := serve(t, &fakeClient{}, in+list+"\n")
			if len(msgs) != 2 || msgs[0].Error != nil || msgs[1].Error != nil {
				t.Fatalf("responses = %+v", msgs)
			}
			var open map[string]json.RawMessage
			if err := json.Unmarshal(msgs[0].Result, &open); err != nil {
				t.Fatal(err)
			}
			var tools map[string]json.RawMessage
			if err := json.Unmarshal(msgs[1].Result, &tools); err != nil {
				t.Fatal(err)
			}
			if v == modernVersion {
				modernResultFields(t, msgs[0].Result)
				modernResultFields(t, msgs[1].Result)
				if string(tools["ttlMs"]) != "3600000" || string(tools["cacheScope"]) != `"public"` {
					t.Errorf("tools/list lacks caching hints: %s", msgs[1].Result)
				}
				return
			}
			if string(open["protocolVersion"]) != `"`+v+`"` {
				t.Errorf("protocolVersion = %s", open["protocolVersion"])
			}
			// Legacy results are unchanged: no modern members.
			for _, k := range []string{"resultType", "_meta", "ttlMs", "cacheScope"} {
				if _, ok := tools[k]; ok {
					t.Errorf("legacy tools/list has %s: %s", k, msgs[1].Result)
				}
			}
		})
	}
}

func TestModernToolCall(t *testing.T) {
	c := &fakeClient{status: sampleStatus(domain.PressureNormal, scheduler.ModeEnforce, 3)}
	m := one(t, c, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"throt_status","arguments":{},`+modernMeta+`}}`+"\n")
	if m.Error != nil {
		t.Fatalf("err %+v", m.Error)
	}
	r := modernResultFields(t, m.Result)
	if !strings.Contains(string(r["content"]), "pressure: normal") || len(r["structuredContent"]) == 0 {
		t.Errorf("result = %s", m.Result)
	}
}

func TestModernErrors(t *testing.T) {
	meta := func(inner string) string { return `"_meta":{` + inner + `}` }
	caps := `"io.modelcontextprotocol/clientCapabilities":{}`
	tests := []struct {
		name, method, params string
		wantCode             int
		wantRequested        string
	}{
		{"unknown version", "tools/list", meta(`"io.modelcontextprotocol/protocolVersion":"1900-01-01",` + caps), codeUnsupportedVersion, "1900-01-01"},
		{"future version", "server/discover", meta(`"io.modelcontextprotocol/protocolVersion":"2099-01-01",` + caps), codeUnsupportedVersion, "2099-01-01"},
		// A legacy revision is reached with initialize, not per request.
		{"legacy version in _meta", "tools/list", meta(`"io.modelcontextprotocol/protocolVersion":"2025-11-25",` + caps), codeUnsupportedVersion, "2025-11-25"},
		{"version not a string", "tools/list", meta(`"io.modelcontextprotocol/protocolVersion":20260728,` + caps), codeInvalidParams, ""},
		{"missing capabilities", "tools/list", meta(`"io.modelcontextprotocol/protocolVersion":"2026-07-28"`), codeInvalidParams, ""},
		{"capabilities not an object", "tools/call", meta(`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":[]`), codeInvalidParams, ""},
		{"discover without _meta", "server/discover", `"x":1`, codeInvalidParams, ""},
		{"discover with other _meta", "server/discover", meta(`"progressToken":1`), codeInvalidParams, ""},
		// The modern revision removed ping.
		{"modern ping", "ping", modernMeta, codeMethodNotFound, ""},
		{"modern unknown method", "resources/list", modernMeta, codeMethodNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := one(t, &fakeClient{}, `{"jsonrpc":"2.0","id":7,"method":"`+tt.method+`","params":{`+tt.params+`}}`+"\n")
			if m.Error == nil || m.Error.Code != tt.wantCode || string(m.ID) != "7" || m.Result != nil {
				t.Fatalf("got %+v", m)
			}
			if tt.wantCode != codeUnsupportedVersion {
				return
			}
			// rpcError.Data decodes as a generic map.
			data, _ := m.Error.Data.(map[string]any)
			supported, _ := data["supported"].([]any)
			if data["requested"] != tt.wantRequested || len(supported) != 4 || supported[0] != modernVersion {
				t.Errorf("data = %v", m.Error.Data)
			}
		})
	}
}

func TestDualEraOneStream(t *testing.T) {
	in := strings.Join([]string{
		// initialize selects the legacy handshake even with modern _meta.
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28",` + modernMeta + `}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + modernMeta + `}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"ping"}`,
	}, "\n") + "\n"
	msgs := serve(t, &fakeClient{}, in)
	if len(msgs) != 4 {
		t.Fatalf("got %d responses", len(msgs))
	}
	if !strings.Contains(string(msgs[0].Result), `"protocolVersion":"2025-11-25"`) {
		t.Errorf("initialize = %s", msgs[0].Result)
	}
	if !strings.Contains(string(msgs[1].Result), `"resultType":"complete"`) {
		t.Errorf("modern tools/list = %s", msgs[1].Result)
	}
	if strings.Contains(string(msgs[2].Result), "resultType") || string(msgs[3].Result) != "{}" {
		t.Errorf("legacy = %s / %s", msgs[2].Result, msgs[3].Result)
	}
}
