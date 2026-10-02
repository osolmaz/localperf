package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCPUModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(path, []byte("processor\t: 0\nmodel name\t: Fictional CPU X1\nflags\t: fpu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readCPUModel(path); got != "Fictional CPU X1" {
		t.Fatalf("cpu model = %q, want Fictional CPU X1", got)
	}
	if got := readCPUModel(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("cpu model for missing file = %q, want empty", got)
	}
}

func TestParseNvidiaGPUList(t *testing.T) {
	gpus := parseNvidiaGPUList("NVIDIA GB10, [N/A], 580.126.09\nNVIDIA A100, 81920 MiB, 550.0\nshort\n")
	if len(gpus) != 2 {
		t.Fatalf("gpus = %d, want 2", len(gpus))
	}
	if gpus[0].Name != "NVIDIA GB10" || gpus[0].VRAMGiB != 0 || gpus[0].Driver != "580.126.09" {
		t.Fatalf("gpu 0 = %+v, want GB10 with unknown VRAM", gpus[0])
	}
	if gpus[1].Name != "NVIDIA A100" || gpus[1].VRAMGiB != 80 {
		t.Fatalf("gpu 1 = %+v, want A100 with 80 GiB", gpus[1])
	}
}

func TestParseMiBField(t *testing.T) {
	if got := parseMiBField(" 121850 MiB"); got < 118 || got > 120 {
		t.Fatalf("parseMiBField = %f, want ~119 GiB", got)
	}
	if got := parseMiBField("[N/A]"); got != 0 {
		t.Fatalf("parseMiBField N/A = %f, want 0", got)
	}
}
