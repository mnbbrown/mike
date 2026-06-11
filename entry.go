package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Source int

const (
	SrcStdout Source = iota
	SrcStderr
)

// Entry is a single log line, with its JSON decoded if the line was JSON.
type Entry struct {
	ID     uint64
	Raw    string
	Source Source
	Time   time.Time
	JSON   map[string]any // nil if the line is not a JSON object
	lower  string         // lowercased raw line, for substring matching
}

var entryID atomic.Uint64

func NewEntry(raw string, src Source) Entry {
	e := Entry{ID: entryID.Add(1), Raw: raw, Source: src, Time: time.Now(), lower: strings.ToLower(raw)}
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var m map[string]any
		if dec.Decode(&m) == nil && m != nil {
			e.JSON = m
		}
	}
	return e
}

type op int

const (
	opEq op = iota // contains, or numeric equality when both sides are numbers
	opGT
	opGE
	opLT
	opLE
)

// term is one clause of a search. Plain words match anywhere in the line;
// key=value / key>value terms match against that JSON field's value.
type term struct {
	key   string // empty for plain-word terms
	op    op
	val   string // lowercased
	num   float64
	isNum bool // val parses as a number → compare numerically
}

var operators = []struct {
	s  string
	op op
}{
	{">=", opGE}, {"<=", opLE}, {">", opGT}, {"<", opLT}, {"=", opEq},
}

// parseSearch splits a search string into terms. Words are separated by
// spaces and all of them must match (AND). A word containing an operator is
// matched against that field, e.g. "service=api latency_ms>200 timeout".
func parseSearch(s string) []term {
	var terms []term
	for f := range strings.FieldsSeq(strings.ToLower(s)) {
		t := term{val: f}
		for _, o := range operators {
			if k, v, ok := strings.Cut(f, o.s); ok && k != "" {
				t = term{key: k, op: o.op, val: v}
				t.num, t.isNum = parseNum(v)
				break
			}
		}
		terms = append(terms, t)
	}
	return terms
}

func parseNum(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}

// toNum extracts a number from a JSON value, including numeric strings.
func toNum(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		return parseNum(x)
	}
	return 0, false
}

// matchValue reports whether one field value satisfies the term. Comparisons
// are numeric when both sides look like numbers, otherwise alphabetical.
func (t term) matchValue(v any) bool {
	if t.isNum {
		if n, ok := toNum(v); ok {
			switch t.op {
			case opEq:
				return n == t.num
			case opGT:
				return n > t.num
			case opGE:
				return n >= t.num
			case opLT:
				return n < t.num
			case opLE:
				return n <= t.num
			}
		}
		if t.op != opEq {
			return false // "field>5" against a non-numeric value
		}
	}
	s := strings.ToLower(formatValue(v))
	switch t.op {
	case opGT:
		return s > t.val
	case opGE:
		return s >= t.val
	case opLT:
		return s < t.val
	case opLE:
		return s <= t.val
	}
	return strings.Contains(s, t.val)
}

// matches reports whether the entry satisfies every search term.
// No terms matches everything.
func (e *Entry) matches(terms []term) bool {
	for _, t := range terms {
		if t.key == "" {
			if !strings.Contains(e.lower, t.val) {
				return false
			}
			continue
		}
		if e.JSON == nil {
			return false
		}
		v, ok := e.JSON[t.key]
		if !ok || !t.matchValue(v) {
			return false
		}
	}
	return true
}

// well-known field names, used for ordering and colouring
var (
	timeKeys  = []string{"time", "ts", "timestamp", "@timestamp", "t"}
	levelKeys = []string{"level", "severity", "lvl", "loglevel"}
	msgKeys   = []string{"msg", "message", "event"}
)

func isOneOf(k string, set []string) bool {
	for _, s := range set {
		if strings.EqualFold(k, s) {
			return true
		}
	}
	return false
}

// sortKeys orders field names with time, level and message first and the
// rest alphabetical.
func sortKeys(keys []string) {
	rank := func(k string) int {
		switch {
		case isOneOf(k, timeKeys):
			return 0
		case isOneOf(k, levelKeys):
			return 1
		case isOneOf(k, msgKeys):
			return 2
		}
		return 3
	}
	sort.SliceStable(keys, func(i, j int) bool {
		ri, rj := rank(keys[i]), rank(keys[j])
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
}

// orderedKeys returns the entry's top-level keys, nicely ordered.
func (e *Entry) orderedKeys() []string {
	keys := make([]string, 0, len(e.JSON))
	for k := range e.JSON {
		keys = append(keys, k)
	}
	sortKeys(keys)
	return keys
}

// Level returns the normalised log level of the entry, or "".
func (e *Entry) Level() string {
	if e.JSON == nil {
		return ""
	}
	for k, v := range e.JSON {
		if isOneOf(k, levelKeys) {
			if s, ok := v.(string); ok {
				return strings.ToLower(s)
			}
		}
	}
	return ""
}

// LevelRank buckets entries for the quick level filters: 3 = errors,
// 2 = warnings, 1 = info-ish, 0 = debug noise. Plain stderr lines count
// as errors; lines with no recognisable level count as info.
func (e *Entry) LevelRank() int {
	switch e.Level() {
	case "error", "fatal", "panic":
		return 3
	case "warn", "warning":
		return 2
	case "debug", "trace":
		return 0
	}
	if e.JSON == nil && e.Source == SrcStderr {
		return 3
	}
	return 1
}

// formatValue renders a JSON value as a plain string (no quotes around strings,
// compact JSON for nested objects/arrays).
func formatValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(t) == nil {
			return strings.TrimRight(buf.String(), "\n")
		}
		return fmt.Sprintf("%v", t)
	}
}
