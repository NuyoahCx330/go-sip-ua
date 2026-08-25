// Package media 提供 RTP 端口管理器。
// 支持动态端口分配/回收，确保 RTP 端口始终为偶数（RTP+RTCP 对）。
package media

import (
	"errors"
	"sync"
	"sync/atomic"
)

// PortManager RTP 端口管理器。
// 管理指定范围内的 UDP 端口分配，确保 RTP/RTCP 端口对为偶数/奇数。
type PortManager struct {
	portMin   int
	portMax   int
	ports     []bool // true = 已分配
	allocated atomic.Int64
	mu        sync.Mutex
}

// PortManagerConfig 端口管理器配置。
type PortManagerConfig struct {
	// PortMin 最小端口号（必须为偶数）。
	PortMin int
	// PortMax 最大端口号。
	PortMax int
}

// DefaultPortManagerConfig 返回默认端口管理器配置。
func DefaultPortManagerConfig() PortManagerConfig {
	return PortManagerConfig{
		PortMin: 10000,
		PortMax: 60000,
	}
}

// NewPortManager 创建端口管理器。
func NewPortManager(cfg PortManagerConfig) (*PortManager, error) {
	if cfg.PortMin <= 0 {
		cfg.PortMin = 10000
	}
	if cfg.PortMax <= cfg.PortMin {
		cfg.PortMax = 60000
	}

	// 确保 PortMin 为偶数
	if cfg.PortMin%2 != 0 {
		cfg.PortMin++
	}

	size := (cfg.PortMax - cfg.PortMin) / 2 // 每对端口占一个槽位
	if size <= 0 {
		return nil, errors.New("media: invalid port range")
	}

	return &PortManager{
		portMin: cfg.PortMin,
		portMax: cfg.PortMax,
		ports:   make([]bool, size),
	}, nil
}

// Allocate 分配一对 RTP/RTCP 端口（偶数/奇数）。
// 返回 RTP 端口号（偶数），RTCP 端口为 RTP+1。
func (pm *PortManager) Allocate() (rtpPort int, err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for i := range pm.ports {
		if !pm.ports[i] {
			pm.ports[i] = true
			pm.allocated.Add(1)
			return pm.portMin + i*2, nil
		}
	}

	return 0, errors.New("media: no available ports")
}

// AllocateN 批量分配 N 对端口。
func (pm *PortManager) AllocateN(n int) ([]int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	ports := make([]int, 0, n)
	for i := range pm.ports {
		if len(ports) >= n {
			break
		}
		if !pm.ports[i] {
			pm.ports[i] = true
			pm.allocated.Add(1)
			ports = append(ports, pm.portMin+i*2)
		}
	}

	if len(ports) < n {
		// 回滚已分配的
		for _, p := range ports {
			idx := (p - pm.portMin) / 2
			pm.ports[idx] = false
			pm.allocated.Add(-1)
		}
		return nil, errors.New("media: insufficient available ports")
	}

	return ports, nil
}

// Release 释放一对端口。
func (pm *PortManager) Release(rtpPort int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if rtpPort < pm.portMin || rtpPort >= pm.portMax {
		return
	}
	if rtpPort%2 != 0 {
		return
	}

	idx := (rtpPort - pm.portMin) / 2
	if idx >= 0 && idx < len(pm.ports) && pm.ports[idx] {
		pm.ports[idx] = false
		pm.allocated.Add(-1)
	}
}

// ReleaseN 批量释放端口。
func (pm *PortManager) ReleaseN(ports []int) {
	for _, p := range ports {
		pm.Release(p)
	}
}

// Available 返回可用端口对数量。
func (pm *PortManager) Available() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	count := 0
	for _, used := range pm.ports {
		if !used {
			count++
		}
	}
	return count
}

// Allocated 返回已分配端口对数量。
func (pm *PortManager) Allocated() int64 {
	return pm.allocated.Load()
}

// Total 返回总端口对数量。
func (pm *PortManager) Total() int {
	return len(pm.ports)
}

// UsagePercent 返回使用率百分比。
func (pm *PortManager) UsagePercent() float64 {
	total := float64(len(pm.ports))
	if total == 0 {
		return 0
	}
	return float64(pm.allocated.Load()) / total * 100
}
