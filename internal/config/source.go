package config

import (
	"encoding/json"
	"os"
)

// Source says where an effective config value came from.
type Source string

// Sources of effective config values.
const (
	SourceDefault Source = "default"
	SourceProfile Source = "profile"
	SourceFile    Source = "file"
)

// Setting is one effective config key, its value, and its source.
type Setting struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source Source `json:"source"`
}

// Effective loads path exactly like Load and attributes each value: a key
// the file sets is SourceFile, a key the applied profile sets is
// SourceProfile, and any other key is SourceDefault. Settings are in Diff's
// key order.
func Effective(path string) (Config, []Setting, error) {
	cfg, err := Load(path)
	if err != nil {
		return cfg, nil, err
	}
	inFile := fileKeys(path)
	byProfile := profileKeys(cfg.Profile)
	var out []Setting
	for _, kv := range flatten(cfg) {
		src := SourceDefault
		switch {
		case inFile[kv.Key]:
			src = SourceFile
		case byProfile[kv.Key]:
			src = SourceProfile
		}
		out = append(out, Setting{Key: kv.Key, Value: kv.Value, Source: src})
	}
	return cfg, out, nil
}

// fileKeys lists the dotted keys the file at path sets. Load has already
// validated it, so a read or parse error here only yields fewer keys.
func fileKeys(path string) map[string]bool {
	keys := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return keys
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return keys
	}
	for k, v := range top {
		if k != "scheduler" {
			keys[k] = true
			continue
		}
		var sched map[string]json.RawMessage
		if json.Unmarshal(v, &sched) == nil {
			for sk := range sched {
				keys["scheduler."+sk] = true
			}
		}
	}
	return keys
}

// profileKeys lists the keys the named profile sets. It applies the profile
// to two configs that differ in every key and records what it overwrote, so
// it stays correct when a profile changes.
func profileKeys(name string) map[string]bool {
	keys := map[string]bool{}
	apply, ok := profiles[name]
	if !ok {
		return keys
	}
	const ncpu = 8 // any count; only which keys change matters
	other := Config{
		SocketPath: "-", SampleInterval: -1, EventHistory: -1, Profile: "-",
		Scheduler: Scheduler{
			Mode: "-", HeavyLimit: -1, StartLimit: -1, ResumeCooldown: -1, PauseOnCritical: true,
			MaxQueueWait: -1, Adaptive: true, PerAgentLimit: -1,
		},
		Agents: []Agent{{Name: "-"}},
	}
	for _, before := range []Config{{}, other} {
		after := before
		apply(&after, ncpu)
		for _, c := range Diff(before, after) {
			keys[c.Key] = true
		}
	}
	return keys
}
