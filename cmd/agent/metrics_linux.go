package main

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// readCPU reads the aggregate counters from the first line of /proc/stat.
func readCPU() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuTimes{}, errors.New("empty /proc/stat")
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, errors.New("unexpected /proc/stat format")
	}
	var t cpuTimes
	for i, s := range fields[1:] {
		if i >= 8 { // guest counters are already included in user/nice
			break
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return cpuTimes{}, err
		}
		t.total += v
		if i == 3 || i == 4 { // idle + iowait
			t.idle += v
		}
	}
	return t, nil
}

func readMem() (float64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	var total, avail float64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, _ = strconv.ParseFloat(f[1], 64)
		case "MemAvailable:":
			avail, _ = strconv.ParseFloat(f[1], 64)
		}
	}
	if total == 0 {
		return 0, errors.New("MemTotal not found")
	}
	return 100 * (total - avail) / total, nil
}

// readDisk counts used space the way df does: used / (used + space available to a non-root user).
func readDisk(path string) (float64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, err
	}
	used := float64(s.Blocks-s.Bfree) * float64(s.Bsize)
	avail := float64(s.Bavail) * float64(s.Bsize)
	if used+avail == 0 {
		return 0, errors.New("partition size is zero")
	}
	return 100 * used / (used + avail), nil
}
