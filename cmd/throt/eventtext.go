package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cfcromn/Throt/internal/domain"
)

// eventTags shortens event kinds for the terminal; others print as is.
var eventTags = map[domain.EventKind]string{
	domain.EventLeaseGranted:      "lease",
	domain.EventLeaseReleased:     "release",
	domain.EventProcessDiscovered: "start",
	domain.EventProcessLost:       "exit",
	domain.EventAgentDiscovered:   "agent+",
	domain.EventAgentLost:         "agent-",
	domain.EventPriorityLowered:   "lower",
	domain.EventPressureChanged:   "pressure",
	domain.EventConfigReloaded:    "config",
}

// eventColors are ANSI colors for tags worth spotting.
var eventColors = map[string]string{
	"queue": "33", "preempt": "33", "pressure": "31", "resume": "32", "lease": "36", "limit": "35",
}

// eventPrinter returns how throt events prints one event: a line per event
// on a terminal (time, a short tag, the message), else a JSON line.
func eventPrinter(w io.Writer, asJSON bool, now time.Time) func(domain.Event) {
	if asJSON || !isTerminalStream(w) {
		return jsonLines(w)
	}
	color := os.Getenv("NO_COLOR") == ""
	return func(e domain.Event) { fmt.Fprintln(w, eventLine(e, now, color)) }
}

// eventLine renders e: "08:37:54  queue     java (claude-16): ...", with
// the date in front when e is not from today.
func eventLine(e domain.Event, now time.Time, color bool) string {
	t := e.Time.Local()
	ts := t.Format("15:04:05")
	if y, m, d := now.Local().Date(); t.Year() != y || t.Month() != m || t.Day() != d {
		ts = t.Format("01-02 ") + ts
	}
	tag := eventTags[e.Kind]
	switch {
	case e.Kind == domain.EventSchedulerDecision && e.Attrs["decision"] != "":
		tag = e.Attrs["decision"]
	case e.Kind == domain.EventSchedulerLimit:
		tag = "limit"
	case tag == "":
		tag = string(e.Kind)
	}
	padded := fmt.Sprintf("%-8s", tag)
	if c := eventColors[tag]; color && c != "" {
		padded = "\x1b[" + c + "m" + padded + "\x1b[0m"
	}
	if color {
		ts = "\x1b[2m" + ts + "\x1b[0m"
	}
	// A decision's message starts with the decision, already the tag.
	return ts + "  " + padded + "  " + strings.TrimPrefix(e.Message, tag+" ")
}

// jsonLines prints each event as one JSON line.
func jsonLines(w io.Writer) func(domain.Event) {
	enc := json.NewEncoder(w)
	return func(e domain.Event) { enc.Encode(e) }
}
