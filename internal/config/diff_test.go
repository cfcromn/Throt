package config

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestDiff(t *testing.T) {
	a := Default()
	b := a
	b.Scheduler.Mode, b.Scheduler.HeavyLimit = "observe", a.Scheduler.HeavyLimit+1
	b.SampleInterval = Duration(5 * time.Second)
	b.Agents = []Agent{{Name: "my", Executables: []string{"x", "y"}}}
	var got []string
	for _, c := range Diff(a, b) {
		got = append(got, c.String())
	}
	want := []string{
		"sample_interval 2s -> 5s",
		"scheduler.mode off -> observe",
		fmt.Sprintf("scheduler.heavy_limit %d -> %d", a.Scheduler.HeavyLimit, b.Scheduler.HeavyLimit),
		"agents [] -> [my(x,y)]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff = %q, want %q", got, want)
	}
	if d := Diff(a, a); len(d) != 0 {
		t.Errorf("Diff(a, a) = %v, want none", d)
	}
}
