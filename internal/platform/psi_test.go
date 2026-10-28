package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cfcromn/Throt/internal/domain"
)

func TestParsePSI(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    domain.Stall
		wantErr bool
	}{
		{
			name: "memory",
			in: "some avg10=12.50 avg60=3.00 avg300=1.00 total=123456\n" +
				"full avg10=4.25 avg60=1.00 avg300=0.20 total=6789\n",
			want: domain.Stall{Some: 0.125, Full: 0.0425},
		},
		{
			name: "idle",
			in:   "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
			want: domain.Stall{},
		},
		{
			name: "cpu before 5.13 has no full line",
			in:   "some avg10=55.00 avg60=20.00 avg300=5.00 total=1\n",
			want: domain.Stall{Some: 0.55},
		},
		{
			name: "field order and unknown fields do not matter",
			in:   "some total=9 future=1 avg10=1.00\nfull avg60=2 avg10=0.50\nnewkind avg10=x\n",
			want: domain.Stall{Some: 0.01, Full: 0.005},
		},
		{
			name: "over 100 percent clamps",
			in:   "some avg10=150.00\nfull avg10=-3\n",
			want: domain.Stall{Some: 1},
		},
		{name: "empty", in: "", wantErr: true},
		{name: "only full", in: "full avg10=1.00\n", wantErr: true},
		{name: "some without avg10", in: "some avg60=1.00 total=3\n", wantErr: true},
		{name: "bad number", in: "some avg10=abc\n", wantErr: true},
		{name: "NaN", in: "some avg10=NaN\n", wantErr: true},
		{name: "bad full", in: "some avg10=1.00\nfull avg10=\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePSI(strings.NewReader(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func psiLine(some, full string) string {
	return "some avg10=" + some + " avg60=0.00 avg300=0.00 total=1\nfull avg10=" + full + " avg60=0.00 avg300=0.00 total=1\n"
}

func TestApplyStalls(t *testing.T) {
	root := t.TempDir()
	host := filepath.Join(root, "pressure")
	writeFile(t, host, "memory", psiLine("1.00", "0.50"))
	writeFile(t, host, "io", psiLine("30.00", "20.00"))
	writeFile(t, host, "cpu", psiLine("60.00", "0.00"))
	memCg := filepath.Join(root, "cg", "mem")
	writeFile(t, memCg, "memory.pressure", psiLine("40.00", "10.00"))
	cpuCg := filepath.Join(root, "cg", "cpu")
	writeFile(t, cpuCg, "cpu.pressure", psiLine("80.00", "5.00"))
	brokenCg := filepath.Join(root, "cg", "broken")
	writeFile(t, brokenCg, "memory.pressure", "garbage\n")

	hostMem := domain.Stall{Some: 0.01, Full: 0.005}
	hostIO := domain.Stall{Some: 0.30, Full: 0.20}
	hostCPU := domain.Stall{Some: 0.60}

	tests := []struct {
		name         string
		hostDir      string
		memCg, cpuCg string
		mem, io, cpu *domain.Stall
		missing      []domain.Metric
		wantErrs     int
	}{
		{name: "host files", hostDir: host, mem: &hostMem, io: &hostIO, cpu: &hostCPU},
		{
			name:    "limiting cgroups replace memory and cpu, io stays host-wide",
			hostDir: host, memCg: memCg, cpuCg: cpuCg,
			mem: &domain.Stall{Some: 0.40, Full: 0.10}, io: &hostIO, cpu: &domain.Stall{Some: 0.80, Full: 0.05},
		},
		{
			name:    "unreadable cgroup files fall back to the host",
			hostDir: host, memCg: brokenCg, cpuCg: filepath.Join(root, "nope"),
			mem: &hostMem, io: &hostIO, cpu: &hostCPU,
		},
		{
			name:     "no PSI: all missing",
			hostDir:  filepath.Join(root, "absent"),
			missing:  []domain.Metric{domain.MetricMemoryStall, domain.MetricIOStall, domain.MetricCPUStall},
			wantErrs: 3,
		},
		{
			name:    "no PSI on the host but a cgroup file",
			hostDir: filepath.Join(root, "absent"), memCg: memCg,
			mem:      &domain.Stall{Some: 0.40, Full: 0.10},
			missing:  []domain.Metric{domain.MetricIOStall, domain.MetricCPUStall},
			wantErrs: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out domain.HostSample
			var fails sampleErrors
			applyStalls(&out, &fails, tt.hostDir, tt.memCg, tt.cpuCg)
			_ = fails.finish(&out)
			check := func(m domain.Metric, want *domain.Stall) {
				got, ok := out.StallFor(m)
				if ok != (want != nil) || (want != nil && got != *want) {
					t.Errorf("%s = %+v (ok %v), want %+v", m, got, ok, want)
				}
			}
			check(domain.MetricMemoryStall, tt.mem)
			check(domain.MetricIOStall, tt.io)
			check(domain.MetricCPUStall, tt.cpu)
			if len(out.Missing) != len(tt.missing) {
				t.Errorf("Missing = %v, want %v", out.Missing, tt.missing)
			}
			if len(fails.errs) != tt.wantErrs {
				t.Errorf("errors = %v, want %d", fails.errs, tt.wantErrs)
			}
		})
	}
}

func TestStallForMissing(t *testing.T) {
	st := &domain.Stall{Some: 0.2}
	s := domain.HostSample{MemoryStall: st, IOStall: st}
	if _, ok := s.StallFor(domain.MetricMemoryStall); !ok {
		t.Error("memory stall set but not reported")
	}
	if _, ok := s.StallFor(domain.MetricCPUStall); ok {
		t.Error("nil cpu stall reported present")
	}
	s.Missing = []domain.Metric{domain.MetricIOStall}
	if _, ok := s.StallFor(domain.MetricIOStall); ok {
		t.Error("io stall listed missing but reported present")
	}
	if _, ok := s.StallFor(domain.MetricCPU); ok {
		t.Error("non-stall metric reported as a stall")
	}
}

func FuzzParsePSI(f *testing.F) {
	f.Add(psiLine("1.00", "0.50"))
	f.Add("some avg10=NaN\n")
	f.Add("some avg10=1e309\nfull avg10=-0\n")
	f.Fuzz(func(t *testing.T, in string) {
		st, err := parsePSI(strings.NewReader(in))
		if err != nil {
			return
		}
		if st.Some < 0 || st.Some > 1 || st.Full < 0 || st.Full > 1 {
			t.Fatalf("parsePSI(%q) = %+v, want fractions 0..1", in, st)
		}
	})
}
