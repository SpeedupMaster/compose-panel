package server

import "compose-panel/internal/docker"

type dockerContainer = docker.Container

// HostStats is a snapshot of host resource usage.
type HostStats struct {
	CPUPercent float64    `json:"cpuPercent"`
	CPUs       int        `json:"cpus"`
	Load       [3]float64 `json:"load"`
	MemTotal   uint64     `json:"memTotal"`
	MemUsed    uint64     `json:"memUsed"`
	SwapTotal  uint64     `json:"swapTotal"`
	SwapUsed   uint64     `json:"swapUsed"`
	DiskTotal  uint64     `json:"diskTotal"`
	DiskUsed   uint64     `json:"diskUsed"`
	Uptime     int64      `json:"uptime"`
}
