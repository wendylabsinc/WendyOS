package hoststats

import "testing"

func TestParseProcStat(t *testing.T) {
	// cpu  <user> <nice> <system> <idle> <iowait> <irq> <softirq> <steal> ...
	data := []byte("cpu  100 0 200 700 50 0 10 0 0 0\n" +
		"cpu0 50 0 100 350 25 0 5 0 0 0\n" +
		"cpu1 50 0 100 350 25 0 5 0 0 0\n" +
		"intr 12345\n")
	got, err := ParseProcStat(data)
	if err != nil {
		t.Fatalf("ParseProcStat: %v", err)
	}
	// total = 100+0+200+700+50+0+10+0+0+0 = 1060
	if got.TotalJiffies != 1060 {
		t.Errorf("TotalJiffies = %d, want 1060", got.TotalJiffies)
	}
	// idle = idle(700) + iowait(50) = 750
	if got.IdleJiffies != 750 {
		t.Errorf("IdleJiffies = %d, want 750", got.IdleJiffies)
	}
	if got.CPUCount != 2 {
		t.Errorf("CPUCount = %d, want 2", got.CPUCount)
	}
}

func TestParseMemInfo(t *testing.T) {
	data := []byte("MemTotal:       16384000 kB\n" +
		"MemFree:         1000000 kB\n" +
		"MemAvailable:    8192000 kB\n")
	got, err := ParseMemInfo(data)
	if err != nil {
		t.Fatalf("ParseMemInfo: %v", err)
	}
	if got.TotalBytes != 16384000*1024 {
		t.Errorf("TotalBytes = %d, want %d", got.TotalBytes, 16384000*1024)
	}
	if got.AvailableBytes != 8192000*1024 {
		t.Errorf("AvailableBytes = %d, want %d", got.AvailableBytes, 8192000*1024)
	}
}

// Exercise the same counter-delta calculation used by dashboard and CLI clients.
func TestCPUUsageExcludesStealAndGuestDuplicates(t *testing.T) {
	for _, tt := range []struct {
		name   string
		before string
		after  string
		total  uint64
		idle   uint64
	}{
		{"idle VM waiting for host", "cpu 0 0 0 600 0 0 0 400 0 0", "cpu 0 0 0 1200 0 0 0 800 0 0", 1000, 1000},
		{"busy VM waiting for host", "cpu 100 0 100 400 0 0 0 400 0 0", "cpu 200 0 200 800 0 0 0 800 0 0", 1000, 800},
		{"guest time already in user and nice", "cpu 200 100 100 600 0 0 0 0 150 50", "cpu 400 200 200 1200 0 0 0 0 300 100", 1000, 600},
		{"legacy counters without steal", "cpu 100 0 100 800", "cpu 200 0 200 1600", 1000, 800},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before, err := ParseProcStat([]byte(tt.before + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			after, err := ParseProcStat([]byte(tt.after + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			total := after.TotalJiffies - before.TotalJiffies
			idle := after.IdleJiffies - before.IdleJiffies
			if total != tt.total || idle != tt.idle {
				t.Fatalf("counter deltas = total %d, idle %d; want total %d, idle %d", total, idle, tt.total, tt.idle)
			}
			usage := 100 * float64(total-idle) / float64(total)
			want := 100 * float64(tt.total-tt.idle) / float64(tt.total)
			if usage != want {
				t.Fatalf("CPU usage = %v, want %v", usage, want)
			}
		})
	}
}
