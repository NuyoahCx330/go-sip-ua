// Package management 提供库管理和监控接口。
// 支持 Prometheus 指标导出、健康检查和运行时统计。
package management

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics 指标收集器。
type Metrics struct {
	counters   map[string]*atomic.Int64
	gauges     map[string]*atomic.Int64
	histograms map[string]*Histogram
	mu         sync.RWMutex
}

// Histogram 简单直方图。
type Histogram struct {
	Buckets []float64
	Counts  []atomic.Int64
	Sum     atomic.Int64
	Count   atomic.Int64
}

// NewMetrics 创建指标收集器。
func NewMetrics() *Metrics {
	return &Metrics{
		counters:   make(map[string]*atomic.Int64),
		gauges:     make(map[string]*atomic.Int64),
		histograms: make(map[string]*Histogram),
	}
}

// Counter 获取或创建计数器。
func (m *Metrics) Counter(name string) *atomic.Int64 {
	m.mu.RLock()
	if c, ok := m.counters[name]; ok {
		m.mu.RUnlock()
		return c
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.counters[name]; ok {
		return c
	}
	c := &atomic.Int64{}
	m.counters[name] = c
	return c
}

// Gauge 获取或创建仪表。
func (m *Metrics) Gauge(name string) *atomic.Int64 {
	m.mu.RLock()
	if g, ok := m.gauges[name]; ok {
		m.mu.RUnlock()
		return g
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.gauges[name]; ok {
		return g
	}
	g := &atomic.Int64{}
	m.gauges[name] = g
	return g
}

// NewHistogram 创建直方图。
func (m *Metrics) NewHistogram(name string, buckets []float64) *Histogram {
	m.mu.Lock()
	defer m.mu.Unlock()

	h := &Histogram{
		Buckets: buckets,
		Counts:  make([]atomic.Int64, len(buckets)+1),
	}
	m.histograms[name] = h
	return h
}

// Observe 观察一个值。
func (h *Histogram) Observe(value float64) {
	h.Count.Add(1)
	h.Sum.Add(int64(value * 1000))

	for i, b := range h.Buckets {
		if value <= b {
			h.Counts[i].Add(1)
			return
		}
	}
	h.Counts[len(h.Buckets)].Add(1)
}

// ExportPrometheus 导出 Prometheus 格式指标。
func (m *Metrics) ExportPrometheus() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var sb strings.Builder

	// 计数器
	names := make([]string, 0, len(m.counters))
	for name := range m.counters {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		c := m.counters[name]
		sb.WriteString("# TYPE ")
		sb.WriteString(name)
		sb.WriteString(" counter\n")
		sb.WriteString(name)
		fmt.Fprintf(&sb, " %d\n", c.Load())
	}

	// 仪表
	gnames := make([]string, 0, len(m.gauges))
	for name := range m.gauges {
		gnames = append(gnames, name)
	}
	sort.Strings(gnames)

	for _, name := range gnames {
		g := m.gauges[name]
		sb.WriteString("# TYPE ")
		sb.WriteString(name)
		sb.WriteString(" gauge\n")
		sb.WriteString(name)
		fmt.Fprintf(&sb, " %d\n", g.Load())
	}

	// 直方图
	for name, h := range m.histograms {
		sb.WriteString("# TYPE ")
		sb.WriteString(name)
		sb.WriteString(" histogram\n")
		for i, b := range h.Buckets {
			fmt.Fprintf(&sb, "%s_bucket{le=\"%.3f\"} %d\n", name, b, h.Counts[i].Load())
		}
		fmt.Fprintf(&sb, "%s_bucket{le=\"+Inf\"} %d\n", name, h.Counts[len(h.Buckets)].Load())
		fmt.Fprintf(&sb, "%s_sum %d\n", name, h.Sum.Load())
		fmt.Fprintf(&sb, "%s_count %d\n", name, h.Count.Load())
	}

	return sb.String()
}

// ---- 健康检查 ----

// HealthChecker 健康检查器。
type HealthChecker struct {
	checks []Check
	mu     sync.RWMutex
}

// Check 健康检查项。
type Check struct {
	Name    string
	Check   func() error
	Timeout time.Duration
}

// HealthStatus 健康状态。
type HealthStatus struct {
	Status   string        `json:"status"`
	Checks   []CheckStatus `json:"checks"`
	Duration time.Duration `json:"duration"`
}

// CheckStatus 单个检查项状态。
type CheckStatus struct {
	Name     string        `json:"name"`
	Status   string        `json:"status"`
	Message  string        `json:"message,omitempty"`
	Duration time.Duration `json:"duration"`
}

// NewHealthChecker 创建健康检查器。
func NewHealthChecker() *HealthChecker {
	return &HealthChecker{}
}

// AddCheck 添加健康检查项。
func (hc *HealthChecker) AddCheck(name string, check func() error, timeout time.Duration) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.checks = append(hc.checks, Check{Name: name, Check: check, Timeout: timeout})
}

// Run 执行所有健康检查。
func (hc *HealthChecker) Run() *HealthStatus {
	start := time.Now()
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	status := &HealthStatus{Status: "healthy"}

	for _, check := range hc.checks {
		cs := CheckStatus{Name: check.Name}
		checkStart := time.Now()

		err := check.Check()
		cs.Duration = time.Since(checkStart)

		if err != nil {
			cs.Status = "unhealthy"
			cs.Message = err.Error()
			status.Status = "unhealthy"
		} else {
			cs.Status = "healthy"
		}

		status.Checks = append(status.Checks, cs)
	}

	status.Duration = time.Since(start)
	return status
}

// ---- 运行时信息 ----

// RuntimeInfo 运行时信息。
type RuntimeInfo struct {
	GoVersion    string `json:"go_version"`
	NumGoroutine int    `json:"num_goroutine"`
	NumCPU       int    `json:"num_cpu"`
	MemAlloc     uint64 `json:"mem_alloc_bytes"`
	MemTotal     uint64 `json:"mem_total_bytes"`
	MemSys       uint64 `json:"mem_sys_bytes"`
	NumGC        uint32 `json:"num_gc"`
	Uptime       string `json:"uptime"`
}

// GetRuntimeInfo 获取运行时信息。
func GetRuntimeInfo(startTime time.Time) RuntimeInfo {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return RuntimeInfo{
		GoVersion:    runtime.Version(),
		NumGoroutine: runtime.NumGoroutine(),
		NumCPU:       runtime.NumCPU(),
		MemAlloc:     m.Alloc,
		MemTotal:     m.TotalAlloc,
		MemSys:       m.Sys,
		NumGC:        m.NumGC,
		Uptime:       time.Since(startTime).String(),
	}
}

// ---- HTTP 管理服务器 ----

// Server HTTP 管理服务器。
type Server struct {
	mux     *http.ServeMux
	metrics *Metrics
	health  *HealthChecker
	start   time.Time
}

// NewServer 创建管理服务器。
func NewServer(metrics *Metrics, health *HealthChecker) *Server {
	s := &Server{
		mux:     http.NewServeMux(),
		metrics: metrics,
		health:  health,
		start:   time.Now(),
	}

	// 注册端点
	s.mux.HandleFunc("/metrics", s.handleMetrics)
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/runtime", s.handleRuntime)
	s.mux.HandleFunc("/debug/stats", s.handleStats)

	return s
}

// Handler 返回 HTTP handler。
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ListenAndServe 启动管理服务器。
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write([]byte(s.metrics.ExportPrometheus()))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := s.health.Run()
	if status.Status == "healthy" {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	fmt.Fprintf(w, `{"status":"%s","duration":"%s"}`, status.Status, status.Duration)
}

func (s *Server) handleRuntime(w http.ResponseWriter, r *http.Request) {
	info := GetRuntimeInfo(s.start)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"go_version":"%s","goroutines":%d,"cpus":%d,"mem_alloc":%d,"uptime":"%s"}`,
		info.GoVersion, info.NumGoroutine, info.NumCPU, info.MemAlloc, info.Uptime)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "Uptime: %s\n", time.Since(s.start))
	fmt.Fprintf(w, "Goroutines: %d\n", runtime.NumGoroutine())
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Fprintf(w, "Memory Alloc: %d bytes\n", m.Alloc)
	fmt.Fprintf(w, "Memory Sys: %d bytes\n", m.Sys)
	fmt.Fprintf(w, "GC Runs: %d\n", m.NumGC)
}
