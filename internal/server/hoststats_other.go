//go:build !linux

package server

import "runtime"

func hostStats(string) HostStats { return HostStats{CPUs: runtime.NumCPU()} }
