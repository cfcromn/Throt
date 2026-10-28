package tui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/cfcromn/Throt/internal/domain"
)

// styles is one model's style set. Every styled element also carries its
// meaning in text (a label, a marker or a word), so the plain set loses no
// information.
type styles struct {
	title, header, selected, banner, degraded, footer lipgloss.Style
	pressure                                          [domain.PressureCritical + 1]lipgloss.Style
	event                                             [eventClassCount]lipgloss.Style
}

// colorEnabled reports whether the environment allows ANSI styling: not
// when NO_COLOR is set to anything non-empty (https://no-color.org), nor on
// TERM=dumb.
func colorEnabled(getenv func(string) string) bool {
	return getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
}

// newStyles returns the style set: plain when colored is false, else colors
// picked for a dark or a light background so that warnings keep their
// contrast on both.
func newStyles(colored, dark bool) styles {
	var s styles
	if !colored {
		// Zero styles render their text unchanged, without escape codes.
		return s
	}
	ld := lipgloss.LightDark(dark)
	c := func(light, dark string) color.Color { return ld(lipgloss.Color(light), lipgloss.Color(dark)) }
	red := c("124", "9")
	yellow := c("130", "11")
	orange := c("166", "208")
	green := c("28", "10")
	grey := c("242", "247")
	s.title = lipgloss.NewStyle().Bold(true)
	s.header = lipgloss.NewStyle().Faint(true)
	s.selected = lipgloss.NewStyle().Reverse(true)
	s.banner = lipgloss.NewStyle().Bold(true).Foreground(red)
	s.degraded = lipgloss.NewStyle().Foreground(yellow)
	s.footer = lipgloss.NewStyle().Faint(true)
	s.pressure = [...]lipgloss.Style{
		domain.PressureUnknown:  lipgloss.NewStyle().Bold(true).Foreground(grey),
		domain.PressureNormal:   lipgloss.NewStyle().Bold(true).Foreground(green),
		domain.PressureElevated: lipgloss.NewStyle().Bold(true).Foreground(yellow),
		domain.PressureHigh:     lipgloss.NewStyle().Bold(true).Foreground(orange),
		domain.PressureCritical: lipgloss.NewStyle().Bold(true).Foreground(red),
	}
	s.event = [...]lipgloss.Style{
		eventOther:     {},
		eventControl:   lipgloss.NewStyle().Foreground(c("25", "12")),
		eventScheduler: lipgloss.NewStyle().Foreground(c("90", "13")),
		eventFailure:   lipgloss.NewStyle().Bold(true).Foreground(red),
	}
	return s
}

// eventClass groups timeline events so that control, scheduler and failure
// events stand apart. Each class has a text tag as well as a color.
type eventClass int

const (
	eventOther eventClass = iota
	eventControl
	eventScheduler
	eventFailure
	eventClassCount
)

// eventTags are the timeline tags per class, all eventTagWidth wide.
var eventTags = [eventClassCount]string{"     ", "CTL  ", "SCHED", "FAIL "}

const eventTagWidth = 5

// classify returns the class of an event kind. Failure kinds win, and an
// unknown kind that says it failed is a failure too.
func classify(k domain.EventKind) eventClass {
	switch k {
	case domain.EventControlFailed, domain.EventControlRefused, domain.EventDegraded,
		domain.EventConfigReloadFailed, domain.EventOrphanStopped:
		return eventFailure
	case domain.EventControlPaused, domain.EventControlResumed:
		return eventControl
	case domain.EventSchedulerDecision, domain.EventSchedulerLimit,
		domain.EventLeaseGranted, domain.EventLeaseReleased:
		return eventScheduler
	}
	if s := string(k); strings.Contains(s, "fail") || strings.Contains(s, "refused") {
		return eventFailure
	}
	return eventOther
}
