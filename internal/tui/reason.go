package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// reasonCodes maps whole pressure reason codes to short human text.
var reasonCodes = map[string]string{
	"no_signals":       "no signals reported yet",
	"below_thresholds": "all signals below thresholds",
	"recovered":        "signals back below exit thresholds",
}

// thresholdKind says how the threshold in a "signal>=threshold" reason is
// shown.
type thresholdKind int

const (
	thresholdRaw     thresholdKind = iota // as the daemon wrote it
	thresholdPercent                      // a 0..1 fraction shown as a percentage
	thresholdRate                         // bytes per second
)

// reasonSignals maps the signal of a "signal>=threshold" reason code to a
// format taking the threshold.
var reasonSignals = map[string]struct {
	kind   thresholdKind
	format string
}{
	"memory_used":            {thresholdPercent, "memory %s+ used"},
	"cpu":                    {thresholdPercent, "CPU %s+ busy"},
	"load":                   {thresholdRaw, "load ≥%s per CPU"},
	"latency":                {thresholdRaw, "foreground lag ≥%s"},
	"swap_growth":            {thresholdRate, "swapping fast (≥%s)"},
	"kernel_memory_pressure": {thresholdRaw, "kernel memory pressure %s"},
}

// explainReason turns a pressure reason code from throtd into short human
// text. ok is false for codes it does not know, which come back unchanged.
func explainReason(code string) (text string, ok bool) {
	if s, ok := reasonCodes[code]; ok {
		return s, true
	}
	name, th, found := strings.Cut(code, ">=")
	sig, known := reasonSignals[name]
	if !found || !known || th == "" {
		return code, false
	}
	if sig.kind != thresholdRaw {
		v, err := strconv.ParseFloat(th, 64)
		if err != nil || !(v >= 0) || math.IsInf(v, 0) {
			return code, false
		}
		if sig.kind == thresholdPercent {
			th = strconv.FormatFloat(math.Round(v*1000)/10, 'f', -1, 64) + "%"
		} else {
			th = formatRate(v)
		}
	}
	return fmt.Sprintf(sig.format, th), true
}

// formatRate renders bytes per second.
func formatRate(v float64) string {
	switch {
	case v >= 1<<30:
		return strconv.FormatFloat(math.Round(v/(1<<30)*10)/10, 'f', -1, 64) + " GiB/s"
	case v >= 1<<20:
		return strconv.FormatFloat(math.Round(v/(1<<20)*10)/10, 'f', -1, 64) + " MiB/s"
	}
	return strconv.FormatFloat(math.Round(v/(1<<10)*10)/10, 'f', -1, 64) + " KiB/s"
}
