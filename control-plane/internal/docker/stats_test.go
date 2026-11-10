package docker

import "testing"

func TestParseStats(t *testing.T) {
	st, err := ParseStats([]byte(`{"CPUPerc":"12.50%","MemUsage":"1.5MiB / 10GiB","MemPerc":"0.01%","NetIO":"3kB / 1.2kB","BlockIO":"0B / 4.1MB","PIDs":"7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if st.CPUPercent != 12.5 || st.MemUsageBytes != 1572864 || st.MemLimitBytes != 10<<30 ||
		st.NetRxBytes != 3000 || st.NetTxBytes != 1200 || st.BlockRead != 0 || st.BlockWrite != 4100000 || st.PIDs != 7 {
		t.Fatalf("unexpected stats: %+v", st)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0B": 0, "1KiB": 1024, "2kB": 2000, "1.5GiB": 1610612736, "": 0, "junk": 0} {
		if got := ParseSize(in); got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
}
