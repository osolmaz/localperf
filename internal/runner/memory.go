package runner

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

type MemorySnapshot struct {
	MemTotalGiB     float64 `json:"mem_total_gib"`
	MemAvailableGiB float64 `json:"mem_available_gib"`
	SwapFreeGiB     float64 `json:"swap_free_gib"`
}

type MemoryFloorError struct {
	Snapshot MemorySnapshot
	MinGiB   float64
}

func (err *MemoryFloorError) Error() string {
	return fmt.Sprintf("MemAvailable %.1f GiB is below floor %.1f GiB", err.Snapshot.MemAvailableGiB, err.MinGiB)
}

func IsMemoryFloorError(err error) bool {
	var floorErr *MemoryFloorError
	return errors.As(err, &floorErr)
}

var checkMemoryFloor = CheckMemoryFloor

func ReadMemorySnapshot() (MemorySnapshot, error) {
	if runtime.GOOS == "darwin" {
		return readDarwinMemorySnapshot()
	}
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemorySnapshot{}, err
	}
	defer file.Close()
	return ParseMeminfo(file)
}

func ParseMeminfo(reader io.Reader) (MemorySnapshot, error) {
	values := map[string]float64{}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		kib, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		values[key] = kib / 1024 / 1024
	}
	if err := scanner.Err(); err != nil {
		return MemorySnapshot{}, err
	}
	if values["MemAvailable"] <= 0 {
		return MemorySnapshot{}, fmt.Errorf("MemAvailable not found in meminfo")
	}
	return MemorySnapshot{
		MemTotalGiB:     values["MemTotal"],
		MemAvailableGiB: values["MemAvailable"],
		SwapFreeGiB:     values["SwapFree"],
	}, nil
}

func readDarwinMemorySnapshot() (MemorySnapshot, error) {
	return readDarwinMemorySnapshotWith(
		darwinTotalMemoryBytes,
		func() ([]byte, error) { return exec.Command("vm_stat").Output() },
	)
}

func readDarwinMemorySnapshotWith(
	readTotal func() (uint64, error),
	readVMStat func() ([]byte, error),
) (MemorySnapshot, error) {
	totalBytes, err := readTotal()
	if err != nil {
		return MemorySnapshot{}, err
	}
	vmStat, err := readVMStat()
	if err != nil {
		return MemorySnapshot{}, err
	}
	availableBytes, err := parseDarwinVMStat(vmStat)
	if err != nil {
		return MemorySnapshot{}, err
	}
	return MemorySnapshot{
		MemTotalGiB:     bytesToGiB(float64(totalBytes)),
		MemAvailableGiB: bytesToGiB(float64(availableBytes)),
	}, nil
}

func darwinTotalMemoryBytes() (uint64, error) {
	return parseDarwinTotalMemoryBytes(exec.Command("sysctl", "-n", "hw.memsize").Output())
}

func parseDarwinTotalMemoryBytes(out []byte, commandErr error) (uint64, error) {
	if commandErr != nil {
		return 0, commandErr
	}
	total, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse hw.memsize: %w", err)
	}
	return total, nil
}

func parseDarwinVMStat(data []byte) (uint64, error) {
	lines := strings.Split(string(data), "\n")
	pageSize := uint64(0)
	availablePages := uint64(0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if pageSize == 0 {
			pageSize = parseDarwinPageSize(line)
		}
		name, pages, ok := parseDarwinVMStatLine(line)
		if !ok {
			continue
		}
		switch name {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			availablePages += pages
		}
	}
	if pageSize == 0 {
		return 0, fmt.Errorf("page size not found in vm_stat")
	}
	if availablePages == 0 {
		return 0, fmt.Errorf("available pages not found in vm_stat")
	}
	return availablePages * pageSize, nil
}

func parseDarwinPageSize(line string) uint64 {
	const marker = "page size of "
	start := strings.Index(line, marker)
	if start < 0 {
		return 0
	}
	start += len(marker)
	end := strings.Index(line[start:], " ")
	if end < 0 {
		return 0
	}
	value, err := strconv.ParseUint(line[start:start+end], 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func parseDarwinVMStatLine(line string) (string, uint64, bool) {
	name, rawValue, ok := strings.Cut(line, ":")
	if !ok {
		return "", 0, false
	}
	rawValue = strings.Trim(strings.TrimSpace(rawValue), ".")
	rawValue = strings.ReplaceAll(rawValue, ",", "")
	pages, err := strconv.ParseUint(rawValue, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(name), pages, true
}

func bytesToGiB(bytes float64) float64 {
	return bytes / 1024 / 1024 / 1024
}

func CheckMemoryFloor(minGiB float64) (MemorySnapshot, error) {
	snapshot, err := ReadMemorySnapshot()
	if err != nil {
		return snapshot, err
	}
	if snapshot.MemAvailableGiB < minGiB {
		return snapshot, &MemoryFloorError{Snapshot: snapshot, MinGiB: minGiB}
	}
	return snapshot, nil
}
