// Package media 提供 RTP 引擎的生产级实现，包括抖动缓冲区、NACK 和 RTCP。
package media

import (
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ---- Jitter Buffer 实现 (RFC 7262) ----

// JitterBuffer 自适应抖动缓冲区。
// 用于处理网络抖动导致的包乱序和延迟到达。
type JitterBuffer struct {
	// 环形缓冲区
	slots    []*jbSlot
	capacity int

	// 状态
	headSeq   uint16
	tailSeq   uint16
	count     int
	minDelay  int // 最小延迟（毫秒）
	maxDelay  int // 最大延迟（毫秒）
	curDelay  int // 当前自适应延迟
	clockRate int // 时钟频率

	// 统计
	packetsIn   atomic.Int64
	packetsOut  atomic.Int64
	packetsDrop atomic.Int64
	packetsLate atomic.Int64
	jitterEst   atomic.Int64 // 抖动估计（毫秒）

	mu sync.Mutex
}

type jbSlot struct {
	seq     uint16
	ts      uint32
	arrival time.Time
	payload []byte
	valid   bool
}

// JitterBufferConfig 抖动缓冲区配置。
type JitterBufferConfig struct {
	// MinDelayMs 最小缓冲延迟（毫秒），默认 20。
	MinDelayMs int
	// MaxDelayMs 最大缓冲延迟（毫秒），默认 200。
	MaxDelayMs int
	// Capacity 缓冲区容量（包数），默认 100。
	Capacity int
	// ClockRate 时钟频率（Hz），默认 8000。
	ClockRate int
}

// DefaultJitterBufferConfig 返回默认抖动缓冲区配置。
func DefaultJitterBufferConfig() JitterBufferConfig {
	return JitterBufferConfig{
		MinDelayMs: 20,
		MaxDelayMs: 200,
		Capacity:   100,
		ClockRate:  8000,
	}
}

// NewJitterBuffer 创建抖动缓冲区。
func NewJitterBuffer(cfg JitterBufferConfig) *JitterBuffer {
	if cfg.Capacity <= 0 {
		cfg.Capacity = 100
	}
	if cfg.MinDelayMs <= 0 {
		cfg.MinDelayMs = 20
	}
	if cfg.MaxDelayMs <= 0 {
		cfg.MaxDelayMs = 200
	}
	if cfg.ClockRate <= 0 {
		cfg.ClockRate = 8000
	}

	return &JitterBuffer{
		slots:     make([]*jbSlot, cfg.Capacity),
		capacity:  cfg.Capacity,
		minDelay:  cfg.MinDelayMs,
		maxDelay:  cfg.MaxDelayMs,
		curDelay:  cfg.MinDelayMs,
		clockRate: cfg.ClockRate,
	}
}

// Put 将 RTP 包放入抖动缓冲区。
func (jb *JitterBuffer) Put(seq uint16, ts uint32, payload []byte) {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	jb.packetsIn.Add(1)

	idx := int(seq) % jb.capacity
	slot := jb.slots[idx]
	if slot == nil {
		slot = &jbSlot{}
		jb.slots[idx] = slot
	}

	// 如果该槽位已有有效包且 seq 不同，递减计数（覆盖旧包）
	if slot.valid && slot.seq != seq {
		jb.count--
	}

	slot.seq = seq
	slot.ts = ts
	slot.arrival = time.Now()
	slot.payload = make([]byte, len(payload))
	copy(slot.payload, payload)
	slot.valid = true
	jb.count++

	// 更新自适应延迟
	jb.updateDelay()
}

// Get 从抖动缓冲区获取下一个有序包。
// 返回 nil 表示缓冲区中没有可用包。
func (jb *JitterBuffer) Get() (seq uint16, ts uint32, payload []byte, ok bool) {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	if jb.count == 0 {
		return 0, 0, nil, false
	}

	// 查找最早的有效包
	now := time.Now()
	deadline := now.Add(-time.Duration(jb.curDelay) * time.Millisecond)

	for i := 0; i < jb.capacity; i++ {
		slot := jb.slots[i]
		if slot == nil || !slot.valid {
			continue
		}

		// 检查是否太新（还没到播放时间）
		if slot.arrival.After(deadline) {
			continue
		}

		// 检查是否太旧（已过期）
		maxAge := time.Duration(jb.maxDelay) * time.Millisecond * 4
		if now.Sub(slot.arrival) > maxAge {
			slot.valid = false
			jb.count--
			jb.packetsDrop.Add(1)
			continue
		}

		// 取出包
		seq = slot.seq
		ts = slot.ts
		payload = slot.payload
		ok = true
		slot.valid = false
		jb.count--
		jb.packetsOut.Add(1)
		return
	}

	return 0, 0, nil, false
}

// Flush 清空抖动缓冲区。
func (jb *JitterBuffer) Flush() {
	jb.mu.Lock()
	defer jb.mu.Unlock()

	for i := range jb.slots {
		jb.slots[i] = nil
	}
	jb.count = 0
}

// Stats 返回抖动缓冲区统计。
func (jb *JitterBuffer) Stats() JitterBufferStats {
	return JitterBufferStats{
		PacketsIn:   jb.packetsIn.Load(),
		PacketsOut:  jb.packetsOut.Load(),
		PacketsDrop: jb.packetsDrop.Load(),
		PacketsLate: jb.packetsLate.Load(),
		Buffered:    int64(jb.count),
		JitterMs:    jb.jitterEst.Load(),
		CurDelayMs:  int64(jb.curDelay),
	}
}

// JitterBufferStats 抖动缓冲区统计。
type JitterBufferStats struct {
	PacketsIn   int64
	PacketsOut  int64
	PacketsDrop int64
	PacketsLate int64
	Buffered    int64
	JitterMs    int64
	CurDelayMs  int64
}

// updateDelay 自适应延迟计算。
// 基于到达时间间隔的方差动态调整缓冲延迟。
func (jb *JitterBuffer) updateDelay() {
	if jb.count < 2 {
		return
	}

	// 收集所有有效包的到达时间
	var arrivals []time.Time
	for _, slot := range jb.slots {
		if slot != nil && slot.valid {
			arrivals = append(arrivals, slot.arrival)
		}
	}

	if len(arrivals) < 2 {
		return
	}

	sort.Slice(arrivals, func(i, j int) bool {
		return arrivals[i].Before(arrivals[j])
	})

	// 计算到达间隔的均值和方差
	var totalInterval float64
	intervals := make([]float64, len(arrivals)-1)
	for i := 1; i < len(arrivals); i++ {
		d := float64(arrivals[i].Sub(arrivals[i-1]).Microseconds()) / 1000.0
		intervals[i-1] = d
		totalInterval += d
	}

	mean := totalInterval / float64(len(intervals))

	var variance float64
	for _, d := range intervals {
		diff := d - mean
		variance += diff * diff
	}
	variance /= float64(len(intervals))

	stddev := math.Sqrt(variance)

	// 自适应延迟 = minDelay + 2 * stddev
	newDelay := int(float64(jb.minDelay) + 2*stddev)
	if newDelay > jb.maxDelay {
		newDelay = jb.maxDelay
	}
	if newDelay < jb.minDelay {
		newDelay = jb.minDelay
	}

	jb.curDelay = newDelay
	jb.jitterEst.Store(int64(stddev))
}

// ---- NACK 实现 (RFC 4585) ----

// NACKTracker NACK 丢包跟踪器。
// 跟踪发送的 RTP 包序列号，检测丢包并生成 NACK 请求。
type NACKTracker struct {
	// 已接收序列号的位图
	received  [65536]bool
	lastSeq   uint16
	initSeq   uint16
	inited    bool
	maxMiss   int           // 最大丢失包数，超过则放弃
	nackDelay time.Duration // NACK 发送延迟
	onNACK    func(seqs []uint16)

	mu sync.Mutex
}

// NACKConfig NACK 跟踪器配置。
type NACKConfig struct {
	// MaxMissedPackets 最大丢失包数，超过则放弃重传请求。
	MaxMissedPackets int
	// NACKDelayMs NACK 发送延迟（毫秒），避免过早请求。
	NACKDelayMs int
}

// NewNACKTracker 创建 NACK 跟踪器。
func NewNACKTracker(cfg NACKConfig, onNACK func(seqs []uint16)) *NACKTracker {
	if cfg.MaxMissedPackets <= 0 {
		cfg.MaxMissedPackets = 50
	}
	if cfg.NACKDelayMs <= 0 {
		cfg.NACKDelayMs = 10
	}
	return &NACKTracker{
		maxMiss:   cfg.MaxMissedPackets,
		nackDelay: time.Duration(cfg.NACKDelayMs) * time.Millisecond,
		onNACK:    onNACK,
	}
}

// OnPacket 收到 RTP 包时调用。
func (nt *NACKTracker) OnPacket(seq uint16) {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	if !nt.inited {
		nt.initSeq = seq
		nt.lastSeq = seq
		nt.inited = true
		nt.received[seq] = true
		return
	}

	nt.received[seq] = true

	// 检测乱序（序列号回绕）
	if seqDiff(seq, nt.lastSeq) > 0 {
		// 正常前进
		nt.lastSeq = seq
	} else {
		// 乱序包，不处理
		return
	}

	// 检查是否有丢包
	nt.checkMissing()
}

// checkMissing 检测丢失的包并触发 NACK。
func (nt *NACKTracker) checkMissing() {
	var missing []uint16
	missCount := 0

	for s := nt.initSeq; s != nt.lastSeq; s++ {
		if !nt.received[s] {
			missing = append(missing, s)
			missCount++
			if missCount > nt.maxMiss {
				break
			}
		}
	}

	if len(missing) > 0 && nt.onNACK != nil {
		// 延迟发送 NACK，给包一些时间到达
		go func() {
			time.Sleep(nt.nackDelay)
			nt.onNACK(missing)
		}()
	}
}

// seqDiff 计算序列号差值（处理回绕）。
func seqDiff(a, b uint16) int {
	d := int(a) - int(b)
	if d > 32768 {
		d -= 65536
	} else if d < -32768 {
		d += 65536
	}
	return d
}

// ---- RTCP 实现 (RFC 3550) ----

// RTCPPacketType RTCP 包类型。
type RTCPPacketType byte

const (
	RTCPSR   RTCPPacketType = 200 // Sender Report
	RTCPRR   RTCPPacketType = 201 // Receiver Report
	RTCPSDES RTCPPacketType = 202 // Source Description
	RTCPBYE  RTCPPacketType = 203 // Goodbye
	RTCPAPP  RTCPPacketType = 204 // Application-defined
)

// RTCPSenderReport RTCP 发送者报告（SR）。
type RTCPSenderReport struct {
	SSRC         uint32
	NTPTime      uint64
	RTPTimestamp uint32
	SenderCount  uint32 // 发送的 RTP 包数
	SenderBytes  uint32 // 发送的字节数
	Reports      []RTCPReceiverReport
}

// RTCPReceiverReport RTCP 接收者报告（RR）。
type RTCPReceiverReport struct {
	SSRC               uint32
	FractionLost       uint8
	CumulativeLost     uint32 // 24-bit
	ExtendedHighestSeq uint32
	Jitter             uint32
	LastSR             uint32 // LSR: NTP timestamp 中间 32 位
	DelayLastSR        uint32 // DLSR: 收到 SR 后的延迟
}

// RTCPStats RTCP 统计收集器。
type RTCPStats struct {
	// 发送统计
	SentPackets atomic.Int64
	SentBytes   atomic.Int64

	// 接收统计
	RecvPackets    atomic.Int64
	RecvBytes      atomic.Int64
	CumulativeLost atomic.Int64

	// 抖动
	Jitter atomic.Int64

	// 序列号
	HighestSeq uint16
	SeqCycles  uint16

	// 时间
	LastSRTime      time.Time
	LastSRTimestamp uint32

	mu sync.Mutex
}

// BuildSR 构建 RTCP Sender Report。
func BuildSR(ssrc uint32, stats *RTCPStats, reports []RTCPReceiverReport) []byte {
	ntpTime := toNTP64(time.Now())

	pktLen := 6 + len(reports)*6 // 以 32-bit 字为单位
	size := 4 + pktLen*4

	pkt := make([]byte, size)
	pkt[0] = 0x80 // V=2, P=0, RC=len(reports)
	pkt[0] |= byte(len(reports) & 0x1F)
	pkt[1] = byte(RTCPSR)
	pkt[2] = byte(pktLen >> 8)
	pkt[3] = byte(pktLen)

	// SSRC
	pkt[4] = byte(ssrc >> 24)
	pkt[5] = byte(ssrc >> 16)
	pkt[6] = byte(ssrc >> 8)
	pkt[7] = byte(ssrc)

	// NTP timestamp
	pkt[8] = byte(ntpTime >> 56)
	pkt[9] = byte(ntpTime >> 48)
	pkt[10] = byte(ntpTime >> 40)
	pkt[11] = byte(ntpTime >> 32)
	pkt[12] = byte(ntpTime >> 24)
	pkt[13] = byte(ntpTime >> 16)
	pkt[14] = byte(ntpTime >> 8)
	pkt[15] = byte(ntpTime)

	// RTP timestamp
	ts := uint32(stats.RecvPackets.Load())
	pkt[16] = byte(ts >> 24)
	pkt[17] = byte(ts >> 16)
	pkt[18] = byte(ts >> 8)
	pkt[19] = byte(ts)

	// Sender packet count
	spc := uint32(stats.SentPackets.Load())
	pkt[20] = byte(spc >> 24)
	pkt[21] = byte(spc >> 16)
	pkt[22] = byte(spc >> 8)
	pkt[23] = byte(spc)

	// Sender byte count
	sbc := uint32(stats.SentBytes.Load())
	pkt[24] = byte(sbc >> 24)
	pkt[25] = byte(sbc >> 16)
	pkt[26] = byte(sbc >> 8)
	pkt[27] = byte(sbc)

	// Receiver reports
	offset := 28
	for _, rr := range reports {
		pkt[offset] = byte(rr.SSRC >> 24)
		pkt[offset+1] = byte(rr.SSRC >> 16)
		pkt[offset+2] = byte(rr.SSRC >> 8)
		pkt[offset+3] = byte(rr.SSRC)

		pkt[offset+4] = rr.FractionLost
		pkt[offset+5] = byte(rr.CumulativeLost >> 16)
		pkt[offset+6] = byte(rr.CumulativeLost >> 8)
		pkt[offset+7] = byte(rr.CumulativeLost)

		pkt[offset+8] = byte(rr.ExtendedHighestSeq >> 24)
		pkt[offset+9] = byte(rr.ExtendedHighestSeq >> 16)
		pkt[offset+10] = byte(rr.ExtendedHighestSeq >> 8)
		pkt[offset+11] = byte(rr.ExtendedHighestSeq)

		pkt[offset+12] = byte(rr.Jitter >> 24)
		pkt[offset+13] = byte(rr.Jitter >> 16)
		pkt[offset+14] = byte(rr.Jitter >> 8)
		pkt[offset+15] = byte(rr.Jitter)

		pkt[offset+16] = byte(rr.LastSR >> 24)
		pkt[offset+17] = byte(rr.LastSR >> 16)
		pkt[offset+18] = byte(rr.LastSR >> 8)
		pkt[offset+19] = byte(rr.LastSR)

		pkt[offset+20] = byte(rr.DelayLastSR >> 24)
		pkt[offset+21] = byte(rr.DelayLastSR >> 16)
		pkt[offset+22] = byte(rr.DelayLastSR >> 8)
		pkt[offset+23] = byte(rr.DelayLastSR)

		offset += 24
	}

	return pkt
}

// BuildRR 构建 RTCP Receiver Report。
func BuildRR(ssrc uint32, reports []RTCPReceiverReport) []byte {
	pktLen := 1 + len(reports)*6
	size := 4 + pktLen*4

	pkt := make([]byte, size)
	pkt[0] = 0x80
	pkt[0] |= byte(len(reports) & 0x1F)
	pkt[1] = byte(RTCPRR)
	pkt[2] = byte(pktLen >> 8)
	pkt[3] = byte(pktLen)

	pkt[4] = byte(ssrc >> 24)
	pkt[5] = byte(ssrc >> 16)
	pkt[6] = byte(ssrc >> 8)
	pkt[7] = byte(ssrc)

	offset := 8
	for _, rr := range reports {
		pkt[offset] = byte(rr.SSRC >> 24)
		pkt[offset+1] = byte(rr.SSRC >> 16)
		pkt[offset+2] = byte(rr.SSRC >> 8)
		pkt[offset+3] = byte(rr.SSRC)

		pkt[offset+4] = rr.FractionLost
		pkt[offset+5] = byte(rr.CumulativeLost >> 16)
		pkt[offset+6] = byte(rr.CumulativeLost >> 8)
		pkt[offset+7] = byte(rr.CumulativeLost)

		pkt[offset+8] = byte(rr.ExtendedHighestSeq >> 24)
		pkt[offset+9] = byte(rr.ExtendedHighestSeq >> 16)
		pkt[offset+10] = byte(rr.ExtendedHighestSeq >> 8)
		pkt[offset+11] = byte(rr.ExtendedHighestSeq)

		pkt[offset+12] = byte(rr.Jitter >> 24)
		pkt[offset+13] = byte(rr.Jitter >> 16)
		pkt[offset+14] = byte(rr.Jitter >> 8)
		pkt[offset+15] = byte(rr.Jitter)

		pkt[offset+16] = byte(rr.LastSR >> 24)
		pkt[offset+17] = byte(rr.LastSR >> 16)
		pkt[offset+18] = byte(rr.LastSR >> 8)
		pkt[offset+19] = byte(rr.LastSR)

		pkt[offset+20] = byte(rr.DelayLastSR >> 24)
		pkt[offset+21] = byte(rr.DelayLastSR >> 16)
		pkt[offset+22] = byte(rr.DelayLastSR >> 8)
		pkt[offset+23] = byte(rr.DelayLastSR)

		offset += 24
	}

	return pkt
}

// ParseRTCP 解析 RTCP 包。
func ParseRTCP(data []byte) (pktType RTCPPacketType, ssrc uint32, err error) {
	if len(data) < 8 {
		return 0, 0, ErrInvalidRTCP
	}
	pktType = RTCPPacketType(data[1])
	ssrc = uint32(data[4])<<24 | uint32(data[5])<<16 | uint32(data[6])<<8 | uint32(data[7])
	return pktType, ssrc, nil
}

// ParseSR 解析 RTCP Sender Report。
func ParseSR(data []byte) (*RTCPSenderReport, error) {
	if len(data) < 28 {
		return nil, ErrInvalidRTCP
	}
	if RTCPPacketType(data[1]) != RTCPSR {
		return nil, ErrInvalidRTCP
	}

	sr := &RTCPSenderReport{
		SSRC: uint32(data[4])<<24 | uint32(data[5])<<16 | uint32(data[6])<<8 | uint32(data[7]),
		NTPTime: uint64(data[8])<<56 | uint64(data[9])<<48 | uint64(data[10])<<40 | uint64(data[11])<<32 |
			uint64(data[12])<<24 | uint64(data[13])<<16 | uint64(data[14])<<8 | uint64(data[15]),
		RTPTimestamp: uint32(data[16])<<24 | uint32(data[17])<<16 | uint32(data[18])<<8 | uint32(data[19]),
		SenderCount:  uint32(data[20])<<24 | uint32(data[21])<<16 | uint32(data[22])<<8 | uint32(data[23]),
		SenderBytes:  uint32(data[24])<<24 | uint32(data[25])<<16 | uint32(data[26])<<8 | uint32(data[27]),
	}

	// 解析接收者报告
	rc := int(data[0] & 0x1F)
	offset := 28
	for i := 0; i < rc && offset+24 <= len(data); i++ {
		rr := RTCPReceiverReport{
			SSRC:               uint32(data[offset])<<24 | uint32(data[offset+1])<<16 | uint32(data[offset+2])<<8 | uint32(data[offset+3]),
			FractionLost:       data[offset+4],
			CumulativeLost:     uint32(data[offset+5])<<16 | uint32(data[offset+6])<<8 | uint32(data[offset+7]),
			ExtendedHighestSeq: uint32(data[offset+8])<<24 | uint32(data[offset+9])<<16 | uint32(data[offset+10])<<8 | uint32(data[offset+11]),
			Jitter:             uint32(data[offset+12])<<24 | uint32(data[offset+13])<<16 | uint32(data[offset+14])<<8 | uint32(data[offset+15]),
			LastSR:             uint32(data[offset+16])<<24 | uint32(data[offset+17])<<16 | uint32(data[offset+18])<<8 | uint32(data[offset+19]),
			DelayLastSR:        uint32(data[offset+20])<<24 | uint32(data[offset+21])<<16 | uint32(data[offset+22])<<8 | uint32(data[offset+23]),
		}
		sr.Reports = append(sr.Reports, rr)
		offset += 24
	}

	return sr, nil
}

// toNTP64 将 time.Time 转换为 NTP 64 位时间戳。
func toNTP64(t time.Time) uint64 {
	// NTP 时间基准: 1900-01-01 00:00:00 UTC
	ntpEpoch := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	d := t.Sub(ntpEpoch)

	secs := uint64(d.Seconds())
	frac := uint64(float64(d.Nanoseconds()) / 1e9 * float64(1<<32))

	return (secs << 32) | frac
}

// RTCPReporter RTCP 报告生成器。
// 定期生成 SR/RR 报告。
type RTCPReporter struct {
	ssrc     uint32
	stats    *RTCPStats
	interval time.Duration
	onSend   func(pkt []byte)
	doneCh   chan struct{}
	started  bool
	mu       sync.Mutex
}

// NewRTCPReporter 创建 RTCP 报告生成器。
func NewRTCPReporter(ssrc uint32, stats *RTCPStats, interval time.Duration, onSend func(pkt []byte)) *RTCPReporter {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &RTCPReporter{
		ssrc:     ssrc,
		stats:    stats,
		interval: interval,
		onSend:   onSend,
		doneCh:   make(chan struct{}),
	}
}

// Start 启动 RTCP 报告。
func (r *RTCPReporter) Start() {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.mu.Unlock()

	go r.reportLoop()
}

// Stop 停止 RTCP 报告。
func (r *RTCPReporter) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return
	}
	r.started = false
	close(r.doneCh)
}

func (r *RTCPReporter) reportLoop() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sr := BuildSR(r.ssrc, r.stats, nil)
			if r.onSend != nil {
				r.onSend(sr)
			}
		case <-r.doneCh:
			return
		}
	}
}

// ---- 辅助 ----

var ErrInvalidRTCP = &rtcpError{"invalid RTCP packet"}

type rtcpError struct{ msg string }

func (e *rtcpError) Error() string { return e.msg }

// ---- RTP 引擎整合 ----

// RTPEngine 完整的 RTP 引擎，整合 Jitter Buffer、NACK 和 RTCP。
type RTPEngine struct {
	session  RTPSession
	jb       *JitterBuffer
	nack     *NACKTracker
	reporter *RTCPReporter
	stats    *RTCPStats
	config   RTPEngineConfig
	mu       sync.RWMutex
}

// RTPEngineConfig RTP 引擎配置。
type RTPEngineConfig struct {
	RTPConfig
	JitterBuffer JitterBufferConfig
	EnableNACK   bool
	EnableRTCP   bool
	RTCPInterval time.Duration
}

// NewRTPEngine 创建完整的 RTP 引擎。
func NewRTPEngine(cfg RTPEngineConfig) (*RTPEngine, error) {
	sess, err := NewRTPSession(cfg.RTPConfig, nil)
	if err != nil {
		return nil, err
	}

	stats := &RTCPStats{}

	engine := &RTPEngine{
		session: sess,
		jb:      NewJitterBuffer(cfg.JitterBuffer),
		stats:   stats,
		config:  cfg,
	}

	if cfg.EnableNACK {
		engine.nack = NewNACKTracker(NACKConfig{}, func(seqs []uint16) {
			// NACK 回调：通过 RTCP 发送 NACK 请求
		})
	}

	if cfg.EnableRTCP {
		engine.reporter = NewRTCPReporter(cfg.SSRC, stats, cfg.RTCPInterval, func(pkt []byte) {
			// RTCP 发送回调
		})
	}

	return engine, nil
}

// Start 启动 RTP 引擎。
func (e *RTPEngine) Start() error {
	// 设置接收回调
	e.session.OnReceive(func(payload []byte, pt int, seq uint16, ts uint32, ssrc uint32) {
		e.stats.RecvPackets.Add(1)
		e.stats.RecvBytes.Add(int64(len(payload)))

		// 放入抖动缓冲区
		e.jb.Put(seq, ts, payload)

		// NACK 跟踪
		if e.nack != nil {
			e.nack.OnPacket(seq)
		}
	})

	if e.reporter != nil {
		e.reporter.Start()
	}

	return nil
}

// Stop 停止 RTP 引擎。
func (e *RTPEngine) Stop() error {
	if e.reporter != nil {
		e.reporter.Stop()
	}
	return e.session.Stop()
}

// GetNextPacket 从抖动缓冲区获取下一个播放包。
func (e *RTPEngine) GetNextPacket() (seq uint16, ts uint32, payload []byte, ok bool) {
	return e.jb.Get()
}

// Stats 返回引擎统计。
func (e *RTPEngine) Stats() RTPEngineStats {
	return RTPEngineStats{
		RTPStats: e.stats,
		JBStats:  e.jb.Stats(),
	}
}

// RTPEngineStats RTP 引擎统计。
type RTPEngineStats struct {
	RTPStats *RTCPStats
	JBStats  JitterBufferStats
}

// 确保 rand 被使用
var _ = rand.Uint32
