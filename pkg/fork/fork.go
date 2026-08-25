// Package fork 实现 RTP 流复制功能，支持动态指定目标地址，用于合法监听、录音、会议桥接等场景。
package fork

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Direction 复制方向。
type Direction int

const (
	ForkIncoming Direction = iota
	ForkOutgoing
	ForkBoth
)

// String 返回复制方向的可读名称。
func (d Direction) String() string {
	switch d {
	case ForkIncoming:
		return "Incoming"
	case ForkOutgoing:
		return "Outgoing"
	case ForkBoth:
		return "Both"
	default:
		return "Unknown"
	}
}

// Target 复制目标。
type Target struct {
	IP   net.IP
	Port int
	SSRC *uint32 // 可选：SSRC 重写
}

// Addr 返回目标地址字符串。
func (t *Target) Addr() string {
	return net.JoinHostPort(t.IP.String(), fmt.Sprintf("%d", t.Port))
}

// ID 复制实例唯一标识。
type ID string

// Status 复制状态。
type Status struct {
	ID            ID
	Direction     Direction
	Targets       []Target
	Active        bool
	PacketsForked atomic.Int64
	BytesForked   atomic.Int64
	StreamIndex   int
	StartedAt     time.Time
}

// Session RTP 流复制会话接口。
type Session interface {
	StartFork(direction Direction, targets []Target, streamIndex int) (ID, error)
	StopFork(id ID) error
	UpdateTargets(id ID, targets []Target) error
	PauseFork(id ID, pause bool) error
	GetStatus(id ID) (*Status, error)
	StopAllForks() error
	ListForks() []ID
	ProcessPacket(data []byte, direction Direction, streamIndex int)
}

// forkEntry 内部复制实例。
type forkEntry struct {
	id          ID
	direction   Direction
	targets     []Target
	streamIndex int
	active      atomic.Bool
	paused      atomic.Bool
	status      Status
	conn        *net.UDPConn
	mu          sync.RWMutex
}

// session 是 Session 的默认实现。
type session struct {
	forks  sync.Map // map[ID]*forkEntry
	nextID atomic.Uint64
	conn   *net.UDPConn
	mu     sync.RWMutex
}

// NewSession 创建 RTP 流复制会话。
func NewSession() Session {
	return &session{}
}

func (s *session) StartFork(direction Direction, targets []Target, streamIndex int) (ID, error) {
	if len(targets) == 0 {
		return "", errors.New("fork: no targets specified")
	}

	id := ID(fmt.Sprintf("fork-%d", s.nextID.Add(1)))

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return "", fmt.Errorf("fork: listen UDP: %w", err)
	}

	entry := &forkEntry{
		id:          id,
		direction:   direction,
		targets:     make([]Target, len(targets)),
		streamIndex: streamIndex,
		conn:        conn,
	}
	copy(entry.targets, targets)
	entry.active.Store(true)
	entry.status = Status{
		ID:          id,
		Direction:   direction,
		Targets:     targets,
		Active:      true,
		StreamIndex: streamIndex,
		StartedAt:   time.Now(),
	}

	s.forks.Store(id, entry)
	return id, nil
}

func (s *session) StopFork(id ID) error {
	val, ok := s.forks.LoadAndDelete(id)
	if !ok {
		return fmt.Errorf("fork: %s not found", id)
	}
	entry := val.(*forkEntry)
	entry.active.Store(false)
	if entry.conn != nil {
		entry.conn.Close()
	}
	return nil
}

func (s *session) UpdateTargets(id ID, targets []Target) error {
	val, ok := s.forks.Load(id)
	if !ok {
		return fmt.Errorf("fork: %s not found", id)
	}
	entry := val.(*forkEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.targets = make([]Target, len(targets))
	copy(entry.targets, targets)
	entry.status.Targets = entry.targets
	return nil
}

func (s *session) PauseFork(id ID, pause bool) error {
	val, ok := s.forks.Load(id)
	if !ok {
		return fmt.Errorf("fork: %s not found", id)
	}
	entry := val.(*forkEntry)
	entry.paused.Store(pause)
	entry.status.Active = !pause
	return nil
}

func (s *session) GetStatus(id ID) (*Status, error) {
	val, ok := s.forks.Load(id)
	if !ok {
		return nil, fmt.Errorf("fork: %s not found", id)
	}
	entry := val.(*forkEntry)
	return &entry.status, nil
}

func (s *session) StopAllForks() error {
	s.forks.Range(func(key, value interface{}) bool {
		entry := value.(*forkEntry)
		entry.active.Store(false)
		if entry.conn != nil {
			entry.conn.Close()
		}
		s.forks.Delete(key)
		return true
	})
	return nil
}

func (s *session) ListForks() []ID {
	var ids []ID
	s.forks.Range(func(key, value interface{}) bool {
		ids = append(ids, key.(ID))
		return true
	})
	return ids
}

func (s *session) ProcessPacket(data []byte, direction Direction, streamIndex int) {
	s.forks.Range(func(key, value interface{}) bool {
		entry := value.(*forkEntry)
		if !entry.active.Load() || entry.paused.Load() {
			return true
		}
		if entry.streamIndex != streamIndex {
			return true
		}
		if entry.direction != ForkBoth && entry.direction != direction {
			return true
		}

		entry.mu.RLock()
		targets := entry.targets
		entry.mu.RUnlock()

		for _, target := range targets {
			addr := &net.UDPAddr{IP: target.IP, Port: target.Port}
			n, err := entry.conn.WriteToUDP(data, addr)
			if err != nil {
				continue
			}
			entry.status.PacketsForked.Add(1)
			entry.status.BytesForked.Add(int64(n))
		}
		return true
	})
}
