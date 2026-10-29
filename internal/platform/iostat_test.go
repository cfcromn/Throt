package platform

import (
	"math"
	"testing"
)

func TestParseIostatLine(t *testing.T) {
	tests := []struct {
		line   string
		busy   float64
		header bool
		ok     bool
	}{
		{"      cpu    load average", 0, true, true},
		{" us sy id   1m   5m   15m", 0, true, true},
		{" 13  9 77  23.58 14.72 9.95", 0.23, false, true},
		{" 65 22 13  24.57 15.07 10.10", 0.87, false, true},
		{"  0  0 100  0.00 0.00 0.00", 0, false, true},
		{"100  0  0  99.99 50 7", 1, false, true},
		{" 33 33 33  1.00 1.00 1.00", 0.67, false, true}, // rounding: sum 99
		{" 34 34 34  1.00 1.00 1.00", 0.66, false, true}, // rounding: sum 102

		{"", 0, false, false},
		{"cpu", 0, false, false},
		{"us sy id", 0, false, false},
		{" disk0 cpu load average", 0, false, false},
		{" 13  9 77  23.58 14.72", 0, false, false},
		{" 13  9 77  23.58 14.72 9.95 1", 0, false, false},
		{" 50 50 50  1 1 1", 0, false, false},     // sum 150
		{" 10 10 10  1 1 1", 0, false, false},     // sum 30
		{"101  0  0  1 1 1", 0, false, false},     // over 100
		{" -1  1 100 1 1 1", 0, false, false},     // sign
		{" +5  5 90  1 1 1", 0, false, false},     // sign
		{"1000 0 0   1 1 1", 0, false, false},     // too long
		{" 1.5 2 97  1 1 1", 0, false, false},     // not an integer
		{" xx  2 97  1 1 1", 0, false, false},     // not a number
		{"  1  2 97  inf 1 1", 0, false, false},   // inf
		{"  1  2 97  NaN 1 1", 0, false, false},   // NaN
		{"  1  2 97  1e3 1 1", 0, false, false},   // exponent
		{"  1  2 97  -1 1 1", 0, false, false},    // negative
		{"  1  2 97  1.2.3 1 1", 0, false, false}, // two dots
		{"  1  2 97  . 1 1", 0, false, false},     // a dot alone
		{"  1  2 97  0x10 1 1", 0, false, false},  // hex
	}
	for _, tt := range tests {
		busy, header, err := parseIostatLine(tt.line)
		if (err == nil) != tt.ok || header != tt.header || math.Abs(busy-tt.busy) > 1e-9 {
			t.Errorf("parseIostatLine(%q) = %v, %v, %v; want %v, %v, ok %v", tt.line, busy, header, err, tt.busy, tt.header, tt.ok)
		}
	}
}
