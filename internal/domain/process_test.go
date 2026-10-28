package domain

import (
	"testing"
	"time"
)

func TestWorkloadClassHeavy(t *testing.T) {
	for c, want := range map[WorkloadClass]bool{
		ClassBuild: true, ClassTest: true, ClassContainerBuild: true,
		ClassAgent: false, ClassUnknown: false, "": false,
	} {
		if got := c.Heavy(); got != want {
			t.Errorf("%q.Heavy() = %v, want %v", c, got, want)
		}
	}
}

func TestProcessKey(t *testing.T) {
	start := time.Unix(100, 0)
	p := ProcessSample{PID: 42, PPID: 1, Start: start, Name: "go"}
	if k := p.Key(); k != (ProcessKey{PID: 42, Start: start}) {
		t.Errorf("Key = %+v", k)
	}
	// A reused PID is a different process.
	q := p
	q.Start = start.Add(time.Second)
	if p.Key() == q.Key() {
		t.Error("a reused PID has the same key")
	}
}
