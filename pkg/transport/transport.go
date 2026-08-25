// Package transport 提供 SIP 传输层实现，支持 UDP、TCP、TLS 和 DTLS。
// 包含连接管理、连接池、传输层路由等功能。
package transport

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
)

// Protocol 表示传输协议类型。
type Protocol string

const (
	UDP  Protocol = "UDP"
	TCP  Protocol = "TCP"
	TLS  Protocol = "TLS"
	DTLS Protocol = "DTLS"
	WS   Protocol = "WS"
	WSS  Protocol = "WSS"
)

// Config 传输层配置。
type Config struct {
	// ListenAddr 监听地址
	ListenAddr string
	// ListenPort 监听端口
	ListenPort int
	// Protocols 启用的传输协议
	Protocols []Protocol
	// TLSConfig TLS 配置
	TLSConfig *tls.Config
	// MaxConnections 最大连接数
	MaxConnections int
	// ReadBufferSize UDP 读缓冲区大小
	ReadBufferSize int
	// WriteBufferSize 写缓冲区大小
	WriteBufferSize int
	// ConnectionIdleTimeout 空闲连接超时
	ConnectionIdleTimeout time.Duration
	// ReadTimeout 读超时
	ReadTimeout time.Duration
	// WriteTimeout 写超时
	WriteTimeout time.Duration
}

// DefaultConfig 返回默认传输层配置。
func DefaultConfig() Config {
	return Config{
		ListenAddr:            "0.0.0.0",
		ListenPort:            5060,
		Protocols:             []Protocol{UDP, TCP},
		MaxConnections:        10000,
		ReadBufferSize:        65535,
		WriteBufferSize:       65535,
		ConnectionIdleTimeout: 30 * time.Second,
		ReadTimeout:           30 * time.Second,
		WriteTimeout:          10 * time.Second,
	}
}

// MessageHandler 接收到 SIP 消息时的回调函数。
type MessageHandler func(msg interface{}, src net.Addr, proto Protocol)

// TransportLayer 传输层接口。
type TransportLayer interface {
	// Start 启动传输层，开始监听。
	Start() error
	// Stop 停止传输层，关闭所有连接。
	Stop() error
	// SendMessage 发送 SIP 消息到指定地址。
	SendMessage(msg interface{}, dst net.Addr, proto Protocol) error
	// SendRaw 发送原始字节数据到指定地址（用于 RTP 等非 SIP 数据）。
	SendRaw(data []byte, dst net.Addr, proto Protocol) error
	// SetMessageHandler 设置消息接收处理器。
	SetMessageHandler(handler MessageHandler)
	// GetLocalAddr 获取本地监听地址。
	GetLocalAddr(proto Protocol) (net.Addr, error)
	// IsReliable 判断协议是否可靠（TCP/TLS 为可靠）。
	IsReliable(proto Protocol) bool
	// Stats 获取传输层统计信息。
	Stats() *Stats
}

// Stats 传输层统计信息。
type Stats struct {
	MessagesSent     atomic.Int64
	MessagesReceived atomic.Int64
	SendErrors       atomic.Int64
	RecvErrors       atomic.Int64
	ActiveConns      atomic.Int64
	BytesSent        atomic.Int64
	BytesReceived    atomic.Int64
}

// transportLayer 是 TransportLayer 的默认实现。
type transportLayer struct {
	config  Config
	log     logger.Logger
	handler atomic.Value // MessageHandler，使用 atomic.Value 避免锁竞争
	stats   Stats

	udpConn     *net.UDPConn
	tcpLn       net.Listener
	tlsLn       net.Listener
	listenAddrs map[Protocol]net.Addr

	// TCP 连接池
	tcpConns  sync.Map // map[string]*tcpConn
	connCount atomic.Int64

	wg      sync.WaitGroup
	doneCh  chan struct{}
	mu      sync.RWMutex
	started bool
}

// tcpConn 表示一个 TCP/TLS 连接及其元数据。
type tcpConn struct {
	conn     net.Conn
	proto    Protocol
	lastUsed atomic.Int64
}

// NewTransportLayer 创建新的传输层实例。
func NewTransportLayer(cfg Config, log logger.Logger) TransportLayer {
	if log == nil {
		log = logger.NopLogger()
	}
	return &transportLayer{
		config:      cfg,
		log:         log,
		listenAddrs: make(map[Protocol]net.Addr),
		doneCh:      make(chan struct{}),
	}
}

func (t *transportLayer) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.started {
		return errors.New("transport: already started")
	}

	for _, proto := range t.config.Protocols {
		switch proto {
		case UDP:
			if err := t.startUDP(); err != nil {
				return fmt.Errorf("transport: start UDP: %w", err)
			}
		case TCP:
			if err := t.startTCP(); err != nil {
				return fmt.Errorf("transport: start TCP: %w", err)
			}
		case TLS:
			if err := t.startTLS(); err != nil {
				return fmt.Errorf("transport: start TLS: %w", err)
			}
		}
	}

	t.started = true
	t.log.Info("transport", "transport layer started")
	return nil
}

func (t *transportLayer) Stop() error {
	t.mu.Lock()
	if !t.started {
		t.mu.Unlock()
		return nil
	}
	t.started = false
	t.mu.Unlock()

	close(t.doneCh)

	if t.udpConn != nil {
		t.udpConn.Close()
	}
	if t.tcpLn != nil {
		t.tcpLn.Close()
	}
	if t.tlsLn != nil {
		t.tlsLn.Close()
	}

	// 关闭所有 TCP 连接
	t.tcpConns.Range(func(key, value interface{}) bool {
		if tc, ok := value.(*tcpConn); ok {
			tc.conn.Close()
		}
		t.tcpConns.Delete(key)
		return true
	})

	t.wg.Wait()
	t.log.Info("transport", "transport layer stopped")
	return nil
}

func (t *transportLayer) startUDP() error {
	addr := net.JoinHostPort(t.config.ListenAddr, fmt.Sprintf("%d", t.config.ListenPort))
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}

	t.udpConn = conn
	t.listenAddrs[UDP] = conn.LocalAddr()
	t.log.Info("transport", "UDP listening on %s", conn.LocalAddr())

	t.wg.Add(1)
	go t.readUDP()
	return nil
}

func (t *transportLayer) readUDP() {
	defer t.wg.Done()
	buf := make([]byte, t.config.ReadBufferSize)

	for {
		select {
		case <-t.doneCh:
			return
		default:
		}

		if t.config.ReadTimeout > 0 {
			t.udpConn.SetReadDeadline(time.Now().Add(t.config.ReadTimeout))
		}

		n, remoteAddr, err := t.udpConn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-t.doneCh:
				return
			default:
				t.stats.RecvErrors.Add(1)
				t.log.Error("transport", "UDP read error: %v", err)
				continue
			}
		}

		t.stats.MessagesReceived.Add(1)
		t.stats.BytesReceived.Add(int64(n))

		// 使用 atomic.Value 加载 handler，避免并发读写竞争
		h := t.handler.Load()
		if h != nil {
			handler := h.(MessageHandler)
			data := make([]byte, n)
			copy(data, buf[:n])
			go func(d []byte, addr *net.UDPAddr) {
				msg, err := message.ParseMessage(d)
				if err != nil {
					t.log.Error("transport", "UDP parse message: %v", err)
					return
				}
				handler(msg, addr, UDP)
			}(data, remoteAddr)
		}
	}
}

func (t *transportLayer) startTCP() error {
	addr := net.JoinHostPort(t.config.ListenAddr, fmt.Sprintf("%d", t.config.ListenPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	t.tcpLn = ln
	t.listenAddrs[TCP] = ln.Addr()
	t.log.Info("transport", "TCP listening on %s", ln.Addr())

	t.wg.Add(1)
	go t.acceptTCP()
	return nil
}

func (t *transportLayer) acceptTCP() {
	defer t.wg.Done()

	for {
		select {
		case <-t.doneCh:
			return
		default:
		}

		conn, err := t.tcpLn.Accept()
		if err != nil {
			select {
			case <-t.doneCh:
				return
			default:
				t.log.Error("transport", "TCP accept error: %v", err)
				continue
			}
		}

		if t.config.MaxConnections > 0 && t.connCount.Load() >= int64(t.config.MaxConnections) {
			t.log.Warn("transport", "TCP max connections reached, rejecting %s", conn.RemoteAddr())
			conn.Close()
			continue
		}

		tc := &tcpConn{conn: conn, proto: TCP}
		tc.lastUsed.Store(time.Now().UnixNano())
		key := conn.RemoteAddr().String()
		t.tcpConns.Store(key, tc)
		t.connCount.Add(1)
		t.stats.ActiveConns.Add(1)

		t.wg.Add(1)
		go t.readTCP(tc, key)
	}
}

func (t *transportLayer) readTCP(tc *tcpConn, key string) {
	defer func() {
		tc.conn.Close()
		t.tcpConns.Delete(key)
		t.connCount.Add(-1)
		t.stats.ActiveConns.Add(-1)
		t.wg.Done()
	}()

	buf := make([]byte, t.config.ReadBufferSize)
	var partial []byte
	const maxPartialSize = 2 * 1024 * 1024 // 2MB 最大缓冲区，防止 OOM

	for {
		select {
		case <-t.doneCh:
			return
		default:
		}

		if t.config.ReadTimeout > 0 {
			tc.conn.SetReadDeadline(time.Now().Add(t.config.ReadTimeout))
		}

		n, err := tc.conn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-t.doneCh:
				return
			default:
				if !errors.Is(err, net.ErrClosed) {
					t.log.Debug("transport", "TCP read from %s: %v", key, err)
				}
				return
			}
		}

		tc.lastUsed.Store(time.Now().UnixNano())
		t.stats.MessagesReceived.Add(1)
		t.stats.BytesReceived.Add(int64(n))

		// 将数据追加到 partial 缓冲区
		partial = append(partial, buf[:n]...)

		// 防止 partial 缓冲区无限增长（畸形消息或慢速客户端）
		if len(partial) > maxPartialSize {
			t.log.Warn("transport", "TCP partial buffer exceeded %d bytes from %s, discarding", maxPartialSize, key)
			partial = partial[:0]
			continue
		}

		// 尝试解析完整的 SIP 消息
		for {
			msgData, remaining := extractSIPMessage(partial)
			if msgData == nil {
				break
			}
			partial = remaining

			h := t.handler.Load()
			if h != nil {
				handler := h.(MessageHandler)
				msg, parseErr := message.ParseMessage(msgData)
				if parseErr != nil {
					t.log.Error("transport", "TCP parse message from %s: %v", key, parseErr)
					continue
				}
				handler(msg, tc.conn.RemoteAddr(), tc.proto)
			}
		}
	}
}

func (t *transportLayer) startTLS() error {
	if t.config.TLSConfig == nil {
		return errors.New("transport: TLS config required")
	}

	port := t.config.ListenPort + 1 // TLS 通常使用不同端口
	addr := net.JoinHostPort(t.config.ListenAddr, fmt.Sprintf("%d", port))
	ln, err := tls.Listen("tcp", addr, t.config.TLSConfig)
	if err != nil {
		return err
	}

	t.tlsLn = ln
	t.listenAddrs[TLS] = ln.Addr()
	t.log.Info("transport", "TLS listening on %s", ln.Addr())

	t.wg.Add(1)
	go t.acceptTLS()
	return nil
}

func (t *transportLayer) acceptTLS() {
	defer t.wg.Done()

	for {
		select {
		case <-t.doneCh:
			return
		default:
		}

		conn, err := t.tlsLn.Accept()
		if err != nil {
			select {
			case <-t.doneCh:
				return
			default:
				t.log.Error("transport", "TLS accept error: %v", err)
				continue
			}
		}

		if t.config.MaxConnections > 0 && t.connCount.Load() >= int64(t.config.MaxConnections) {
			conn.Close()
			continue
		}

		tc := &tcpConn{conn: conn, proto: TLS}
		tc.lastUsed.Store(time.Now().UnixNano())
		key := conn.RemoteAddr().String()
		t.tcpConns.Store(key, tc)
		t.connCount.Add(1)
		t.stats.ActiveConns.Add(1)

		t.wg.Add(1)
		go t.readTCP(tc, key)
	}
}

func (t *transportLayer) SendMessage(msg interface{}, dst net.Addr, proto Protocol) error {
	var data []byte
	switch m := msg.(type) {
	case *message.Request:
		data = m.Bytes()
	case *message.Response:
		data = m.Bytes()
	default:
		return errors.New("transport: invalid message type")
	}

	switch proto {
	case UDP:
		return t.sendUDP(data, dst)
	case TCP:
		return t.sendTCP(data, dst, TCP)
	case TLS:
		return t.sendTCP(data, dst, TLS)
	default:
		return fmt.Errorf("transport: unsupported protocol: %s", proto)
	}
}

// SendRaw 发送原始字节数据（用于 RTP 等非 SIP 数据）。
func (t *transportLayer) SendRaw(data []byte, dst net.Addr, proto Protocol) error {
	if len(data) == 0 {
		return nil
	}
	switch proto {
	case UDP:
		return t.sendUDP(data, dst)
	case TCP:
		return t.sendTCP(data, dst, TCP)
	case TLS:
		return t.sendTCP(data, dst, TLS)
	default:
		return fmt.Errorf("transport: unsupported protocol: %s", proto)
	}
}

func (t *transportLayer) sendUDP(data []byte, dst net.Addr) error {
	udpAddr, ok := dst.(*net.UDPAddr)
	if !ok {
		return errors.New("transport: invalid UDP address")
	}
	if t.udpConn == nil {
		return errors.New("transport: UDP not started")
	}

	// UDP WriteToUDP 本身是并发安全的，不需要 SetWriteDeadline
	// （SetWriteDeadline 在并发调用时存在竞争）
	n, err := t.udpConn.WriteToUDP(data, udpAddr)
	if err != nil {
		t.stats.SendErrors.Add(1)
		return err
	}
	t.stats.MessagesSent.Add(1)
	t.stats.BytesSent.Add(int64(n))
	return nil
}

func (t *transportLayer) sendTCP(data []byte, dst net.Addr, proto Protocol) error {
	key := dst.String()
	tc := t.getOrCreateTCPConn(key, dst, proto)
	if tc == nil {
		t.stats.SendErrors.Add(1)
		return fmt.Errorf("transport: failed to connect to %s", key)
	}

	if t.config.WriteTimeout > 0 {
		tc.conn.SetWriteDeadline(time.Now().Add(t.config.WriteTimeout))
	}

	n, err := tc.conn.Write(data)
	if err != nil {
		t.tcpConns.Delete(key)
		tc.conn.Close()
		t.stats.SendErrors.Add(1)
		return err
	}

	tc.lastUsed.Store(time.Now().UnixNano())
	t.stats.MessagesSent.Add(1)
	t.stats.BytesSent.Add(int64(n))
	return nil
}

func (t *transportLayer) getOrCreateTCPConn(key string, dst net.Addr, proto Protocol) *tcpConn {
	if val, ok := t.tcpConns.Load(key); ok {
		return val.(*tcpConn)
	}

	// 创建新连接
	var conn net.Conn
	var err error

	dialer := &net.Dialer{Timeout: 5 * time.Second}

	switch proto {
	case TCP:
		conn, err = dialer.Dial("tcp", dst.String())
	case TLS:
		if t.config.TLSConfig == nil {
			return nil
		}
		conn, err = tls.DialWithDialer(dialer, "tcp", dst.String(), t.config.TLSConfig)
	default:
		return nil
	}

	if err != nil {
		t.log.Error("transport", "failed to connect to %s: %v", key, err)
		return nil
	}

	tc := &tcpConn{conn: conn, proto: proto}
	tc.lastUsed.Store(time.Now().UnixNano())

	actual, _ := t.tcpConns.LoadOrStore(key, tc)
	if actual != tc {
		// 已有其他 goroutine 创建了连接
		conn.Close()
		return actual.(*tcpConn)
	}

	t.connCount.Add(1)
	t.stats.ActiveConns.Add(1)

	t.wg.Add(1)
	go t.readTCP(tc, key)

	return tc
}

func (t *transportLayer) SetMessageHandler(handler MessageHandler) {
	t.handler.Store(handler)
}

func (t *transportLayer) GetLocalAddr(proto Protocol) (net.Addr, error) {
	addr, ok := t.listenAddrs[proto]
	if !ok {
		return nil, fmt.Errorf("transport: protocol %s not started", proto)
	}
	return addr, nil
}

func (t *transportLayer) IsReliable(proto Protocol) bool {
	return proto == TCP || proto == TLS || proto == WS || proto == WSS
}

func (t *transportLayer) Stats() *Stats {
	return &t.stats
}

// extractSIPMessage 从缓冲区中提取一个完整的 SIP 消息。
// 返回消息数据和剩余数据。
func extractSIPMessage(data []byte) ([]byte, []byte) {
	// 查找头域结束标记
	headerEnd := -1
	for i := 0; i < len(data)-3; i++ {
		if data[i] == '\r' && data[i+1] == '\n' && data[i+2] == '\r' && data[i+3] == '\n' {
			headerEnd = i + 4
			break
		}
	}
	if headerEnd < 0 {
		return nil, data
	}

	// 从头域中解析 Content-Length
	header := string(data[:headerEnd])
	contentLength := 0
	for _, line := range strings.Split(header, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &contentLength)
			}
			break
		}
	}

	totalLen := headerEnd + contentLength
	if len(data) < totalLen {
		return nil, data
	}

	return data[:totalLen], data[totalLen:]
}
