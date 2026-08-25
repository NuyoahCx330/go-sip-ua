package media

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
)

// RelaySession 媒体转发会话接口。
type RelaySession interface {
	// Start 启动转发。
	Start(ctx context.Context) error
	// Stop 停止转发。
	Stop() error
	// SetSource 设置源地址（接收端）。
	SetSource(addr *net.UDPAddr)
	// SetDestination 设置目标地址（发送端）。
	SetDestination(addr *net.UDPAddr)
	// Stats 获取转发统计。
	Stats() *RelayStats
	// Pause 暂停/恢复转发。
	Pause(pause bool)
}

// RelayStats 转发统计。
type RelayStats struct {
	PacketsForwarded atomic.Int64
	BytesForwarded   atomic.Int64
	PacketsDropped   atomic.Int64
	StartTime        time.Time
}

// relaySession 是 RelaySession 的默认实现。
type relaySession struct {
	config  RelayConfig
	log     logger.Logger
	source  *net.UDPConn
	dest    *net.UDPAddr
	stats   RelayStats
	paused  atomic.Bool
	doneCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.RWMutex
	started bool
}

// NewRelaySession 创建媒体转发会话。
func NewRelaySession(cfg RelayConfig, log logger.Logger) RelaySession {
	if log == nil {
		log = logger.NopLogger()
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 65535
	}
	return &relaySession{
		config: cfg,
		log:    log,
		doneCh: make(chan struct{}),
	}
}

func (r *relaySession) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("media: relay session already started")
	}
	r.started = true
	r.stats.StartTime = time.Now()
	r.mu.Unlock()

	r.wg.Add(1)
	go r.forwardLoop(ctx)

	r.log.Info("media", "relay session started")
	return nil
}

func (r *relaySession) Stop() error {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = false
	r.mu.Unlock()

	close(r.doneCh)
	if r.source != nil {
		r.source.Close()
	}
	r.wg.Wait()
	return nil
}

func (r *relaySession) SetSource(addr *net.UDPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.source != nil {
		r.source.Close()
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		r.log.Error("media", "relay: listen source %s: %v", addr, err)
		return
	}
	r.source = conn
	r.log.Info("media", "relay: source set to %s", addr)
}

func (r *relaySession) SetDestination(addr *net.UDPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dest = addr
	r.log.Info("media", "relay: destination set to %s", addr)
}

func (r *relaySession) Stats() *RelayStats {
	return &r.stats
}

func (r *relaySession) Pause(pause bool) {
	r.paused.Store(pause)
}

func (r *relaySession) forwardLoop(ctx context.Context) {
	defer r.wg.Done()
	buf := make([]byte, r.config.BufferSize)

	for {
		select {
		case <-r.doneCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		if r.paused.Load() {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		r.mu.RLock()
		src := r.source
		dst := r.dest
		r.mu.RUnlock()

		if src == nil || dst == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		src.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, from, err := src.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-r.doneCh:
				return
			default:
				r.log.Debug("media", "relay: read error: %v", err)
				continue
			}
		}

		if n < 12 {
			r.stats.PacketsDropped.Add(1)
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		// 可选：SSRC 重写
		if r.config.SSRCRewrite {
			rewriteSSRC(data)
		}

		// 可选：时间戳重写
		if r.config.TimestampRewrite {
			rewriteTimestamp(data)
		}

		_, err = src.WriteToUDP(data, dst)
		if err != nil {
			r.stats.PacketsDropped.Add(1)
			r.log.Debug("media", "relay: write error: %v", err)
			continue
		}

		r.stats.PacketsForwarded.Add(1)
		r.stats.BytesForwarded.Add(int64(n))

		_ = from // 避免未使用警告
	}
}

// rewriteSSRC 重写 RTP 包中的 SSRC。
func rewriteSSRC(packet []byte) {
	if len(packet) < 12 {
		return
	}
	// SSRC 在偏移 8-11 字节处
	// 使用新的 SSRC 替换（此处保留原值，实际使用时由调用方设置新值）
	// 生产环境中应传入目标 SSRC 参数
}

// rewriteTimestamp 重写 RTP 包中的时间戳。
func rewriteTimestamp(packet []byte) {
	if len(packet) < 12 {
		return
	}
	// Timestamp 在偏移 4-7 字节处
	// 根据转发延迟调整时间戳
}

// RewriteSSRCWith 使用指定 SSRC 重写 RTP 包。
func RewriteSSRCWith(packet []byte, ssrc uint32) {
	if len(packet) < 12 {
		return
	}
	packet[8] = byte(ssrc >> 24)
	packet[9] = byte(ssrc >> 16)
	packet[10] = byte(ssrc >> 8)
	packet[11] = byte(ssrc)
}

// RewriteTimestampWith 使用指定时间戳重写 RTP 包。
func RewriteTimestampWith(packet []byte, ts uint32) {
	if len(packet) < 12 {
		return
	}
	packet[4] = byte(ts >> 24)
	packet[5] = byte(ts >> 16)
	packet[6] = byte(ts >> 8)
	packet[7] = byte(ts)
}

// RewriteSeqWith 使用指定序列号重写 RTP 包。
func RewriteSeqWith(packet []byte, seq uint16) {
	if len(packet) < 4 {
		return
	}
	packet[2] = byte(seq >> 8)
	packet[3] = byte(seq)
}

// MediaRelay 媒体转发管理器。
type MediaRelay interface {
	// CreateRelay 创建转发会话。
	CreateRelay(cfg RelayConfig) (RelaySession, error)
	// StopAll 停止所有转发会话。
	StopAll() error
}

// mediaRelay 是 MediaRelay 的默认实现。
type mediaRelay struct {
	sessions sync.Map
	log      logger.Logger
}

// NewMediaRelay 创建媒体转发管理器。
func NewMediaRelay(log logger.Logger) MediaRelay {
	if log == nil {
		log = logger.NopLogger()
	}
	return &mediaRelay{log: log}
}

func (mr *mediaRelay) CreateRelay(cfg RelayConfig) (RelaySession, error) {
	session := NewRelaySession(cfg, mr.log)
	id := fmt.Sprintf("relay-%d", time.Now().UnixNano())
	mr.sessions.Store(id, session)
	return session, nil
}

func (mr *mediaRelay) StopAll() error {
	mr.sessions.Range(func(key, value interface{}) bool {
		if session, ok := value.(RelaySession); ok {
			session.Stop()
		}
		mr.sessions.Delete(key)
		return true
	})
	return nil
}
