// Package cdr 提供呼叫详细记录（Call Detail Record）系统。
// 用于运营商级计费、审计、监控和故障排查。
package cdr

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
)

// CallType 呼叫类型。
type CallType int

const (
	CallTypeOriginated CallType = iota // 主叫
	CallTypeTerminated                 // 被叫
	CallTypeForwarded                  // 转接
	CallTypeRedirected                 // 重定向
)

// String 返回呼叫类型的可读名称。
func (ct CallType) String() string {
	switch ct {
	case CallTypeOriginated:
		return "Originated"
	case CallTypeTerminated:
		return "Terminated"
	case CallTypeForwarded:
		return "Forwarded"
	case CallTypeRedirected:
		return "Redirected"
	default:
		return "Unknown"
	}
}

// DisconnectCause 拆线原因。
type DisconnectCause int

const (
	CauseNormalClearing DisconnectCause = 16
	CauseUserBusy       DisconnectCause = 17
	CauseNoUserResponse DisconnectCause = 18
	CauseNoAnswer       DisconnectCause = 19
	CauseRejected       DisconnectCause = 21
	CauseRedirected     DisconnectCause = 22
	CauseUnallocated    DisconnectCause = 1
	CauseNetworkBusy    DisconnectCause = 38
	CauseServiceUnavail DisconnectCause = 63
	CauseInvalidNumber  DisconnectCause = 28
	CauseAuthFailure    DisconnectCause = 95
	CauseInternalError  DisconnectCause = 127
)

// Record 呼叫详细记录。
type Record struct {
	// 基本标识
	CallID   string   `json:"call_id"`
	RecordID string   `json:"record_id"`
	CallType CallType `json:"call_type"`

	// 主被叫信息
	FromUser string `json:"from_user"`
	FromURI  string `json:"from_uri"`
	FromTag  string `json:"from_tag"`
	ToUser   string `json:"to_user"`
	ToURI    string `json:"to_uri"`
	ToTag    string `json:"to_tag"`

	// 网络信息
	SourceIP   string `json:"source_ip"`
	SourcePort int    `json:"source_port"`
	DestIP     string `json:"dest_ip"`
	DestPort   int    `json:"dest_port"`
	Transport  string `json:"transport"`

	// 时间信息
	StartTime     time.Time     `json:"start_time"`
	ConnectTime   time.Time     `json:"connect_time"`
	EndTime       time.Time     `json:"end_time"`
	SetupDuration time.Duration `json:"setup_duration"`
	RingDuration  time.Duration `json:"ring_duration"`
	TalkDuration  time.Duration `json:"talk_duration"`

	// 拆线信息
	DisconnectCause DisconnectCause `json:"disconnect_cause"`
	SIPCode         int             `json:"sip_code"`
	Reason          string          `json:"reason"`
	DisconnectParty string          `json:"disconnect_party"` // "caller" / "callee" / "network"

	// 媒体信息
	Codec           string  `json:"codec"`
	MediaMode       string  `json:"media_mode"`
	PacketsSent     int64   `json:"packets_sent"`
	PacketsReceived int64   `json:"packets_received"`
	PacketsLost     int64   `json:"packets_lost"`
	BytesSent       int64   `json:"bytes_sent"`
	BytesReceived   int64   `json:"bytes_received"`
	MOS             float64 `json:"mos,omitempty"` // Mean Opinion Score

	// 路由信息
	IngressTrunk string `json:"ingress_trunk,omitempty"`
	EgressTrunk  string `json:"egress_trunk,omitempty"`
	RouteGroup   string `json:"route_group,omitempty"`
	CarrierID    string `json:"carrier_id,omitempty"`

	// 计费信息
	RateCenter       string  `json:"rate_center,omitempty"`
	Cost             float64 `json:"cost,omitempty"`
	Currency         string  `json:"currency,omitempty"`
	BillingIncrement int     `json:"billing_increment,omitempty"` // 秒

	// 自定义字段
	CustomFields map[string]string `json:"custom_fields,omitempty"`
}

// Duration 返回总呼叫时长。
func (r *Record) Duration() time.Duration {
	if r.EndTime.IsZero() {
		return time.Since(r.StartTime)
	}
	return r.EndTime.Sub(r.StartTime)
}

// MarshalJSON 序列化为 JSON。
func (r *Record) MarshalJSON() ([]byte, error) {
	type Alias Record
	return json.Marshal(&struct {
		*Alias
		SetupDurationMs int64 `json:"setup_duration_ms"`
		RingDurationMs  int64 `json:"ring_duration_ms"`
		TalkDurationMs  int64 `json:"talk_duration_ms"`
	}{
		Alias:           (*Alias)(r),
		SetupDurationMs: r.SetupDuration.Milliseconds(),
		RingDurationMs:  r.RingDuration.Milliseconds(),
		TalkDurationMs:  r.TalkDuration.Milliseconds(),
	})
}

// Store CDR 存储接口。
type Store interface {
	// Write 写入 CDR 记录。
	Write(record *Record) error
	// WriteBatch 批量写入 CDR 记录。
	WriteBatch(records []*Record) error
	// Query 查询 CDR 记录。
	Query(filter *QueryFilter) ([]*Record, error)
	// Flush 刷新缓冲区。
	Flush() error
	// Close 关闭存储。
	Close() error
}

// QueryFilter CDR 查询过滤条件。
type QueryFilter struct {
	CallID     string
	FromUser   string
	ToUser     string
	StartTime  time.Time
	EndTime    time.Time
	SIPCode    int
	MaxResults int
}

// Manager CDR 管理器。
type Manager interface {
	// StartCall 记录呼叫开始。
	StartCall(callID string, req *message.Request, src net.Addr) *Record
	// UpdateConnect 记录呼叫接通。
	UpdateConnect(callID string, code int, connectTime time.Time)
	// UpdateMedia 更新媒体统计。
	UpdateMedia(callID string, codec string, packetsSent, packetsRecv, packetsLost int64)
	// EndCall 记录呼叫结束。
	EndCall(callID string, cause DisconnectCause, sipCode int, reason string, party string)
	// SetCustomField 设置自定义字段。
	SetCustomField(callID string, key, value string)
	// GetRecord 获取 CDR 记录。
	GetRecord(callID string) *Record
	// GetStats 获取 CDR 统计。
	GetStats() *CDRStats
	// SetStore 设置存储后端。
	SetStore(store Store)
}

// CDRStats CDR 统计信息。
type CDRStats struct {
	TotalCalls     atomic.Int64
	ActiveCalls    atomic.Int64
	CompletedCalls atomic.Int64
	FailedCalls    atomic.Int64
	TotalTalkTime  atomic.Int64 // 纳秒
	TotalSetupTime atomic.Int64 // 纳秒
	ASR            atomic.Int64 // Answer Seizure Ratio * 10000
	ACD            atomic.Int64 // Average Call Duration (毫秒)
}

// manager CDR 管理器实现。
type manager struct {
	records sync.Map // map[string]*Record
	store   Store
	log     logger.Logger
	stats   CDRStats
	nextID  atomic.Uint64
}

// NewManager 创建 CDR 管理器。
func NewManager(log logger.Logger) Manager {
	if log == nil {
		log = logger.NopLogger()
	}
	return &manager{log: log}
}

func (m *manager) StartCall(callID string, req *message.Request, src net.Addr) *Record {
	record := &Record{
		CallID:    callID,
		RecordID:  fmt.Sprintf("cdr-%d", m.nextID.Add(1)),
		CallType:  CallTypeOriginated,
		StartTime: time.Now(),
	}

	if req != nil {
		from := req.From()
		if from != nil && from.Address != nil {
			record.FromUser = from.Address.User
			record.FromURI = from.Address.String()
			record.FromTag = from.Tag()
		}
		to := req.To()
		if to != nil && to.Address != nil {
			record.ToUser = to.Address.User
			record.ToURI = to.Address.String()
		}
		vias := req.Via()
		if len(vias) > 0 {
			record.Transport = vias[0].Transport
		}
	}

	if src != nil {
		if udpAddr, ok := src.(*net.UDPAddr); ok {
			record.SourceIP = udpAddr.IP.String()
			record.SourcePort = udpAddr.Port
		}
	}

	record.CustomFields = make(map[string]string)
	m.records.Store(callID, record)
	m.stats.TotalCalls.Add(1)
	m.stats.ActiveCalls.Add(1)

	m.log.Debug("cdr", "call started: %s from %s to %s", callID, record.FromUser, record.ToUser)
	return record
}

func (m *manager) UpdateConnect(callID string, code int, connectTime time.Time) {
	val, ok := m.records.Load(callID)
	if !ok {
		return
	}
	record := val.(*Record)
	record.ConnectTime = connectTime
	record.SIPCode = code
	record.SetupDuration = connectTime.Sub(record.StartTime)
	m.stats.TotalSetupTime.Add(record.SetupDuration.Nanoseconds())
	m.stats.CompletedCalls.Add(1)
	m.stats.ActiveCalls.Add(-1)
}

func (m *manager) UpdateMedia(callID string, codec string, packetsSent, packetsRecv, packetsLost int64) {
	val, ok := m.records.Load(callID)
	if !ok {
		return
	}
	record := val.(*Record)
	record.Codec = codec
	record.PacketsSent = packetsSent
	record.PacketsReceived = packetsRecv
	record.PacketsLost = packetsLost
}

func (m *manager) EndCall(callID string, cause DisconnectCause, sipCode int, reason string, party string) {
	val, ok := m.records.Load(callID)
	if !ok {
		return
	}
	record := val.(*Record)
	record.EndTime = time.Now()
	record.DisconnectCause = cause
	record.SIPCode = sipCode
	record.Reason = reason
	record.DisconnectParty = party

	if !record.ConnectTime.IsZero() {
		record.TalkDuration = record.EndTime.Sub(record.ConnectTime)
		m.stats.TotalTalkTime.Add(record.TalkDuration.Nanoseconds())
	}

	m.records.Delete(callID)

	// 写入存储
	if m.store != nil {
		if err := m.store.Write(record); err != nil {
			m.log.Error("cdr", "failed to write CDR for %s: %v", callID, err)
		}
	}

	m.log.Debug("cdr", "call ended: %s cause=%d reason=%s duration=%s",
		callID, cause, reason, record.TalkDuration)
}

func (m *manager) SetCustomField(callID string, key, value string) {
	val, ok := m.records.Load(callID)
	if !ok {
		return
	}
	record := val.(*Record)
	if record.CustomFields == nil {
		record.CustomFields = make(map[string]string)
	}
	record.CustomFields[key] = value
}

func (m *manager) GetRecord(callID string) *Record {
	val, ok := m.records.Load(callID)
	if !ok {
		return nil
	}
	return val.(*Record)
}

func (m *manager) GetStats() *CDRStats {
	total := m.stats.TotalCalls.Load()
	completed := m.stats.CompletedCalls.Load()
	if total > 0 {
		m.stats.ASR.Store(completed * 10000 / total)
	}
	if completed > 0 {
		m.stats.ACD.Store(m.stats.TotalTalkTime.Load() / completed / int64(time.Millisecond))
	}
	return &m.stats
}

func (m *manager) SetStore(store Store) {
	m.store = store
}

// ---- MemoryStore 内存存储实现 ----

// MemoryStore 基于内存的 CDR 存储，用于测试和小规模部署。
type MemoryStore struct {
	records []*Record
	mu      sync.RWMutex
	maxSize int
}

// NewMemoryStore 创建内存 CDR 存储。
func NewMemoryStore(maxSize int) *MemoryStore {
	if maxSize <= 0 {
		maxSize = 100000
	}
	return &MemoryStore{maxSize: maxSize}
}

func (s *MemoryStore) Write(record *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) >= s.maxSize {
		// 淘汰最旧记录
		s.records = s.records[1:]
	}
	s.records = append(s.records, record)
	return nil
}

func (s *MemoryStore) WriteBatch(records []*Record) error {
	for _, r := range records {
		if err := s.Write(r); err != nil {
			return err
		}
	}
	return nil
}

func (s *MemoryStore) Query(filter *QueryFilter) ([]*Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []*Record
	for _, r := range s.records {
		if filter.CallID != "" && r.CallID != filter.CallID {
			continue
		}
		if filter.FromUser != "" && r.FromUser != filter.FromUser {
			continue
		}
		if filter.ToUser != "" && r.ToUser != filter.ToUser {
			continue
		}
		if !filter.StartTime.IsZero() && r.StartTime.Before(filter.StartTime) {
			continue
		}
		if !filter.EndTime.IsZero() && r.StartTime.After(filter.EndTime) {
			continue
		}
		if filter.SIPCode > 0 && r.SIPCode != filter.SIPCode {
			continue
		}
		results = append(results, r)
		if filter.MaxResults > 0 && len(results) >= filter.MaxResults {
			break
		}
	}
	return results, nil
}

func (s *MemoryStore) Flush() error { return nil }
func (s *MemoryStore) Close() error { return nil }

// Records 返回所有记录（用于测试）。
func (s *MemoryStore) Records() []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Record, len(s.records))
	copy(result, s.records)
	return result
}
