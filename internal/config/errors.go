package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// parseError explains a decode error of the config file b at path: the
// line and column of a syntax or type error, and for an unknown key its
// section and the closest known key.
func parseError(path string, b []byte, err error) error {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	var dur *durationError
	switch {
	case errors.As(err, &dur):
		if key, off, ok := badDuration(b, 0, ""); ok {
			line, col := position(b, off)
			return fmt.Errorf("parse config %s: line %d, column %d: %s: %w", path, line, col, key, err)
		}
	case errors.As(err, &syn):
		line, col := position(b, syn.Offset)
		return fmt.Errorf("parse config %s: line %d, column %d: %w", path, line, col, err)
	case errors.As(err, &typ):
		line, col := position(b, typ.Offset)
		return fmt.Errorf("parse config %s: line %d, column %d: %w", path, line, col, err)
	case strings.Contains(err.Error(), "unknown field"):
		if msg, ok := unknownKey(b, unknownName(err)); ok {
			return fmt.Errorf("parse config %s: %s: %w", path, msg, err)
		}
	}
	return fmt.Errorf("parse config %s: %w", path, err)
}

// position returns the 1-based line and column of the byte at which
// encoding/json reports an error. Its offset counts the bytes read, so the
// offending byte is the one before it; at the end of input it is the
// position just past the last byte.
func position(b []byte, offset int64) (line, col int) {
	off := int(min(max(offset, 0), int64(len(b))))
	before := b[:off]
	if off > 0 && off < len(b) {
		before = b[:off-1]
	}
	line = 1 + bytes.Count(before, []byte("\n"))
	col = len(before) - bytes.LastIndexByte(before, '\n')
	return line, col
}

// unknownName is the key an "unknown field" error names, or "".
func unknownName(err error) string {
	_, q, ok := strings.Cut(err.Error(), "unknown field ")
	if !ok {
		return ""
	}
	name, uerr := strconv.Unquote(q)
	if uerr != nil {
		return ""
	}
	return name
}

// unknownKey finds the unknown key name (the first unknown key when no
// key is called name) in b and describes it, e.g.
// `unknown key "x" in scheduler; did you mean "y"?`.
func unknownKey(b []byte, name string) (string, bool) {
	var found *badKey
	walkKeys(b, reflect.TypeFor[Config](), "", func(k badKey) {
		if found == nil || (found.key != name && k.key == name) {
			found = &k
		}
	})
	if found == nil {
		return "", false
	}
	where := "in " + found.section
	if found.section == "" {
		where = "at the top level"
	}
	msg := fmt.Sprintf("unknown key %q %s", found.key, where)
	if s := suggest(found.key, found.known); s != "" {
		msg += fmt.Sprintf("; did you mean %q?", s)
	}
	return msg, true
}

// badKey is a key of section that its struct has no field for.
type badKey struct {
	section, key string
	known        []string
}

// walkKeys calls bad for every key of the JSON object b that struct type t
// has no field for, descending into struct fields and slices of structs.
// Keys are visited in sorted order; malformed parts are skipped, since the
// decoder reports those.
func walkKeys(b []byte, t reflect.Type, section string, bad func(badKey)) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil {
		return
	}
	fields := map[string]reflect.Type{}
	var known []string
	for f := range t.Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		fields[tag] = f.Type
		known = append(known, tag)
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		ft, ok := fields[k]
		for tag, typ := range fields {
			// The decoder matches keys case-insensitively.
			if !ok && strings.EqualFold(tag, k) {
				ft, ok = typ, true
			}
		}
		if !ok {
			bad(badKey{section: section, key: k, known: known})
			continue
		}
		sub := k
		if section != "" {
			sub = section + "." + k
		}
		switch {
		case ft.Kind() == reflect.Struct:
			walkKeys(obj[k], ft, sub, bad)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			var items []json.RawMessage
			if json.Unmarshal(obj[k], &items) != nil {
				continue
			}
			for i, item := range items {
				walkKeys(item, ft.Elem(), sub+"["+strconv.Itoa(i)+"]", bad)
			}
		}
	}
}

// suggest returns the known key closest to key when it is close enough to
// be a typo (at most 2 edits, or a quarter of the key for long keys), or "".
func suggest(key string, known []string) string {
	best, bestD := "", max(2, len(key)/4)+1
	for _, k := range known {
		if d := editDistance(key, k); d < bestD {
			best, bestD = k, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, by bytes.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// durationKeys are the dotted keys of the Duration fields, in lower case:
// encoding/json matches keys case-insensitively.
var durationKeys = map[string]bool{
	"sample_interval":           true,
	"scheduler.resume_cooldown": true,
	"scheduler.max_queue_wait":  true,
}

// badDuration returns the dotted key of the first Duration in the JSON
// object b, in document order (where the decoder stops), that does not
// decode, and the offset of its value in the file, where b starts at base.
func badDuration(b []byte, base int64, path string) (key string, offset int64, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", 0, false
	}
	for dec.More() {
		tok, err := dec.Token()
		name, isKey := tok.(string)
		if err != nil || !isKey {
			return "", 0, false
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil || len(raw) == 0 {
			return "", 0, false
		}
		if path != "" {
			name = path + "." + name
		}
		start := base + dec.InputOffset() - int64(len(raw))
		var d Duration
		switch {
		case durationKeys[strings.ToLower(name)]:
			if d.UnmarshalJSON(raw) != nil {
				return name, start + 1, true // position reports the byte before
			}
		case raw[0] == '{':
			if key, off, ok := badDuration(raw, start, name); ok {
				return key, off, true
			}
		}
	}
	return "", 0, false
}
