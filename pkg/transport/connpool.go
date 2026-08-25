// Package transport 提供 SIP 传输层连接池实现。
// 支持 TCP/TLS 连接的复用、LRU 淘汰和健康检查。
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ConnPool TCP/TLS 连接池。
type ConnPool struct {
	maxConns    int           // 最大连接数
	maxIdle     int           // 最大空闲连接数
	idleTimeout time.Duration // 空闲超时
	dialTimeout time.Duration // 连接超时
	readTimeout time.Duration // 读取超时

	// 连接缓存（按地址分组）
	pools sync.Map // map[string]*connList

	// 统计
	stats ConnPoolStats

	mu     sync.Mutex
	closed bool
	doneCh chan struct{}
}

// ConnPoolConfig 连接池配置。
type ConnPoolConfig struct {
	MaxConns    int
	MaxIdle     int
	IdleTimeout time.Duration
	DialTimeout time.Duration
	ReadTimeout time.Duration
}

// DefaultConnPoolConfig 返回默认连接池配置。
func DefaultConnPoolConfig() ConnPoolConfig {
	return ConnPoolConfig{
		MaxConns:    1000,
		MaxIdle:     100,
		IdleTimeout: 90 * time.Second,
		DialTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second,
	}
}

// ConnPoolStats 连接池统计。
type ConnPoolStats struct {
	TotalConns   atomic.Int64
	ActiveConns  atomic.Int64
	IdleConns    atomic.Int64
	TotalGets    atomic.Int64
	TotalPuts    atomic.Int64
	TotalDials   atomic.Int64
	TotalCloses  atomic.Int64
	TotalTimeout atomic.Int64
}

// connList 单个地址的连接列表。
type connList struct {
	conns []*pooledConn
	mu    sync.Mutex
}

// pooledConn 池化连接。
type pooledConn struct {
	net.Conn
	addr     string
	proto    Protocol
	pool     *ConnPool
	lastUsed time.Time
	inPool   bool
}

// NewConnPool 创建连接池。
func NewConnPool(cfg ConnPoolConfig) *ConnPool {
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 1000
	}
	if cfg.MaxIdle <= 0 {
		cfg.MaxIdle = 100
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 90 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}

	cp := &ConnPool{
		maxConns:    cfg.MaxConns,
		maxIdle:     cfg.MaxIdle,
		idleTimeout: cfg.IdleTimeout,
		dialTimeout: cfg.DialTimeout,
		readTimeout: cfg.ReadTimeout,
		doneCh:      make(chan struct{}),
	}

	// 启动空闲连接清理
	go cp.cleanupLoop()

	return cp
}

// Get 获取到指定地址的连接。
func (cp *ConnPool) Get(ctx context.Context, addr string, proto Protocol, tlsCfg *tls.Config) (net.Conn, error) {
	cp.stats.TotalGets.Add(1)

	cp.mu.Lock()
	if cp.closed {
		cp.mu.Unlock()
		return nil, errors.New("connpool: pool closed")
	}
	cp.mu.Unlock()

	key := connKey(addr, proto)

	// 尝试从池中获取空闲连接
	cl := cp.getConnList(key)
	if conn := cl.getIdle(); conn != nil {
		cp.stats.ActiveConns.Add(1)
		return conn, nil
	}

	// 创建新连接
	conn, err := cp.dial(ctx, addr, proto, tlsCfg)
	if err != nil {
		return nil, err
	}

	pc := &pooledConn{
		Conn:     conn,
		addr:     addr,
		proto:    proto,
		pool:     cp,
		lastUsed: time.Now(),
	}

	cp.stats.TotalConns.Add(1)
	cp.stats.ActiveConns.Add(1)
	cp.stats.TotalDials.Add(1)

	return pc, nil
}

// Put 归还连接到池中。
func (cp *ConnPool) Put(conn net.Conn) {
	pc, ok := conn.(*pooledConn)
	if !ok {
		conn.Close()
		return
	}

	cp.stats.TotalPuts.Add(1)
	cp.stats.ActiveConns.Add(-1)

	pc.lastUsed = time.Now()

	// 尝试放回池中
	cl := cp.getConnList(connKey(pc.addr, pc.proto))
	if cl.putIdle(pc, cp.maxIdle) {
		pc.inPool = true
		cp.stats.IdleConns.Add(1)
	} else {
		pc.Conn.Close()
		cp.stats.TotalCloses.Add(1)
		cp.stats.TotalConns.Add(-1)
	}
}

// Close 关闭连接池。
func (cp *ConnPool) Close() error {
	cp.mu.Lock()
	if cp.closed {
		cp.mu.Unlock()
		return nil
	}
	cp.closed = true
	close(cp.doneCh)
	cp.mu.Unlock()

	cp.pools.Range(func(key, value interface{}) bool {
		cl := value.(*connList)
		cl.mu.Lock()
		for _, pc := range cl.conns {
			pc.Conn.Close()
			cp.stats.TotalCloses.Add(1)
		}
		cl.conns = nil
		cl.mu.Unlock()
		return true
	})

	return nil
}

// Stats 返回连接池统计。
func (cp *ConnPool) Stats() *ConnPoolStats {
	return &cp.stats
}

func (cp *ConnPool) dial(ctx context.Context, addr string, proto Protocol, tlsCfg *tls.Config) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: cp.dialTimeout}

	switch proto {
	case TCP:
		return dialer.DialContext(ctx, "tcp", addr)
	case TLS:
		if tlsCfg == nil {
			tlsCfg = &tls.Config{InsecureSkipVerify: true}
		}
		return tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	default:
		return dialer.DialContext(ctx, "tcp", addr)
	}
}

func (cp *ConnPool) getConnList(key string) *connList {
	if v, ok := cp.pools.Load(key); ok {
		return v.(*connList)
	}
	cl := &connList{}
	actual, _ := cp.pools.LoadOrStore(key, cl)
	return actual.(*connList)
}

func (cl *connList) getIdle() *pooledConn {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	for i := len(cl.conns) - 1; i >= 0; i-- {
		pc := cl.conns[i]
		if pc.inPool {
			pc.inPool = false
			cl.conns = append(cl.conns[:i], cl.conns[i+1:]...)
			return pc
		}
	}
	return nil
}

func (cl *connList) putIdle(pc *pooledConn, maxIdle int) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	if len(cl.conns) >= maxIdle {
		return false
	}
	cl.conns = append(cl.conns, pc)
	return true
}

func (cp *ConnPool) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cp.cleanupIdle()
		case <-cp.doneCh:
			return
		}
	}
}

func (cp *ConnPool) cleanupIdle() {
	now := time.Now()
	cp.pools.Range(func(key, value interface{}) bool {
		cl := value.(*connList)
		cl.mu.Lock()
		defer cl.mu.Unlock()

		var active []*pooledConn
		for _, pc := range cl.conns {
			if pc.inPool && now.Sub(pc.lastUsed) > cp.idleTimeout {
				pc.Conn.Close()
				cp.stats.TotalCloses.Add(1)
				cp.stats.TotalConns.Add(-1)
				cp.stats.IdleConns.Add(-1)
				cp.stats.TotalTimeout.Add(1)
			} else {
				active = append(active, pc)
			}
		}
		cl.conns = active
		return true
	})
}

func connKey(addr string, proto Protocol) string {
	return string(proto) + "://" + addr
}
