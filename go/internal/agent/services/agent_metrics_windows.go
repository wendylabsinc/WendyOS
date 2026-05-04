//go:build windows

package services

import "runtime"

func agentCPUNanos() (int64, int64) { return 0, 0 }

func agentMemBytes() int64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapInuse + ms.StackInuse)
}
