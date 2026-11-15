//go:build linux

package server

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var cpuMu sync.Mutex
var lastIdle, lastTotal uint64

func readCPU() (idle, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		fields := strings.Fields(sc.Text())
		for i, v := range fields[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			total += n
			if i == 3 || i == 4 { // idle + iowait
				idle += n
			}
		}
	}
	return
}

func cpuPercent() float64 {
	cpuMu.Lock()
	defer cpuMu.Unlock()
	idle, total := readCPU()
	if lastTotal == 0 {
		lastIdle, lastTotal = idle, total
		time.Sleep(200 * time.Millisecond)
		idle, total = readCPU()
	}
	dIdle, dTotal := float64(idle-lastIdle), float64(total-lastTotal)
	lastIdle, lastTotal = idle, total
	if dTotal <= 0 {
		return 0
	}
	return (1 - dIdle/dTotal) * 100
}

func hostStats(diskPath string) HostStats {
	h := HostStats{CPUs: runtime.NumCPU(), CPUPercent: cpuPercent()}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		for i := 0; i < 3 && i < len(f); i++ {
			h.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		m := map[string]uint64{}
		for _, l := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			n, _ := strconv.ParseUint(strings.Fields(v)[0], 10, 64)
			m[k] = n * 1024
		}
		h.MemTotal = m["MemTotal"]
		h.MemUsed = m["MemTotal"] - m["MemAvailable"]
		h.SwapTotal = m["SwapTotal"]
		h.SwapUsed = m["SwapTotal"] - m["SwapFree"]
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			h.Uptime = int64(v)
		}
	}
	var st syscall.Statfs_t
	if syscall.Statfs(diskPath, &st) == nil {
		h.DiskTotal = st.Blocks * uint64(st.Bsize)
		h.DiskUsed = (st.Blocks - st.Bfree) * uint64(st.Bsize)
	}
	return h
}
