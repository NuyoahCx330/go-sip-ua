// Package sipua 提供运营商级 SIP 协议库的统一入口。
// 整合 UAC/UAS、IMS 注册、媒体处理、传输层、事务管理等核心模块。
//
// 基本使用示例：
//
//	lib, err := sipua.New(&sipua.Config{
//	    Transport: transport.Config{ListenAddr: "0.0.0.0", ListenPort: 5060},
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer lib.Shutdown(context.Background())
package sipua

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/cdr"
	"github.com/NuyoahCx330/go-sip-ua/pkg/fork"
	"github.com/NuyoahCx330/go-sip-ua/pkg/headermanip"
	"github.com/NuyoahCx330/go-sip-ua/pkg/ims"
	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/media"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
	"github.com/NuyoahCx330/go-sip-ua/pkg/proxy"
	"github.com/NuyoahCx330/go-sip-ua/pkg/router"
	"github.com/NuyoahCx330/go-sip-ua/pkg/transaction"
	"github.com/NuyoahCx330/go-sip-ua/pkg/transport"
	"github.com/NuyoahCx330/go-sip-ua/pkg/uac"
	"github.com/NuyoahCx330/go-sip-ua/pkg/uas"
)

// Version 库版本号。
const Version = "0.1.0"

// Config 库全局配置。
type Config struct {
	// Transport 传输层配置
	Transport transport.Config
	// Transaction 事务层配置
	Transaction transaction.Config
	// Log 日志配置
	Log logger.Config
	// Media 媒体配置
	Media MediaConfig
	// Header 自定义头域处理配置
	Header HeaderConfig
	// IMS IMS 配置
	IMS IMSConfig
	// WorkerCount 工作协程数量（默认 CPU 核心数 * 2）
	WorkerCount int
	// QueueSize 任务队列大小
	QueueSize int
	// EnableProfiling 启用性能分析
	EnableProfiling bool
	// GracefulTimeout 优雅关闭超时
	GracefulTimeout time.Duration
}

// MediaConfig 媒体相关配置。
type MediaConfig struct {
	// Mode 媒体处理模式
	Mode media.MediaMode
	// JitterBufferMs 抖动缓冲毫秒数（默认 60ms）
	JitterBufferMs int
	// Codecs 启用的编解码器列表（nil 表示全部启用）
	Codecs []string
	// EnableSRTP 启用 SRTP
	EnableSRTP bool
	// SignalingOnly 仅信令模式配置
	SignalingOnly media.SignalingOnlyConfig
	// Relay 媒体转发模式配置
	Relay media.RelayConfig
	// Transcode 编解码处理模式配置
	Transcode media.TranscodeConfig
}

// HeaderConfig 自定义头域处理配置。
type HeaderConfig struct {
	// Rules 头域处理规则列表
	Rules []*headermanip.Rule
	// EnableManipulator 启用头域操作器
	EnableManipulator bool
}

// IMSConfig IMS 相关配置。
type IMSConfig struct {
	// EnableServer 启用 IMS 服务器模式
	EnableServer bool
	// DefaultExpires 默认注册过期时间（秒）
	DefaultExpires int
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Transport:       transport.DefaultConfig(),
		Transaction:     transaction.DefaultConfig(),
		Log:             logger.DefaultConfig(),
		WorkerCount:     runtime.NumCPU() * 2,
		QueueSize:       10000,
		GracefulTimeout: 10 * time.Second,
		Media: MediaConfig{
			JitterBufferMs: 60,
		},
		IMS: IMSConfig{
			DefaultExpires: 3600,
		},
	}
}

// MemoryStats 内存统计。
type MemoryStats struct {
	Alloc        uint64
	TotalAlloc   uint64
	Sys          uint64
	NumGC        uint32
	PauseTotalNs uint64
}

// Stats 全局统计信息。
type Stats struct {
	UAC            *uac.Stats
	UAS            *uas.Stats
	Transaction    *transaction.Stats
	Transport      *transport.Stats
	Proxy          *proxy.Stats
	IMS            *ims.ServerStats
	Memory         *MemoryStats
	GoroutineCount int
	Uptime         time.Duration
}

// Library 库主入口接口。
type Library interface {
	// Init 初始化库（已在新建时自动调用）。
	Init(config *Config) error
	// Shutdown 优雅关闭库。
	Shutdown(ctx context.Context) error
	// UAC 获取 UAC 接口。
	UAC() uac.UAC
	// UAS 获取 UAS 接口。
	UAS() uas.UAS
	// Registrar 获取 IMS 注册器。
	Registrar() ims.Registrar
	// IMSServer 获取 IMS 服务器接口。
	IMSServer() ims.IMSServer
	// Transport 获取传输层接口。
	Transport() transport.TransportLayer
	// Transaction 获取事务管理器。
	Transaction() transaction.Manager
	// Media 获取媒体引擎。
	Media() media.MediaEngine
	// MediaRelay 获取媒体转发管理器。
	MediaRelay() media.MediaRelay
	// MediaTranscoder 获取转码管理器。
	MediaTranscoder() media.MediaTranscoder
	// Fork 获取 RTP 流复制会话。
	Fork() fork.Session
	// Proxy 获取代理处理器。
	Proxy() proxy.Handler
	// HeaderManipulator 获取头域操作器。
	HeaderManipulator() headermanip.Manipulator
	// CDR 获取 CDR 管理器。
	CDR() cdr.Manager
	// Router 获取 SIP 路由器。
	Router() router.Router
	// Logger 获取日志实例。
	Logger() logger.Logger
	// GetStats 获取全局统计信息。
	GetStats() *Stats
	// GetMediaMode 获取当前媒体处理模式。
	GetMediaMode() media.MediaMode
}

// library 是 Library 接口的默认实现。
type library struct {
	config        Config
	log           logger.Logger
	tp            transport.TransportLayer
	txMgr         transaction.Manager
	uacInst       uac.UAC
	uasInst       uas.UAS
	regInst       ims.Registrar
	imsInst       ims.IMSServer
	mediaInst     media.MediaEngine
	relayInst     media.MediaRelay
	transcodeInst media.MediaTranscoder
	forkInst      fork.Session
	proxyInst     proxy.Handler
	headerManip   headermanip.Manipulator
	cdrMgr        cdr.Manager
	routerInst    router.Router
	startedAt     time.Time
	mu            sync.RWMutex
	closed        bool
}

// New 创建并初始化 SIP 库实例。
func New(config *Config) (Library, error) {
	if config == nil {
		cfg := DefaultConfig()
		config = &cfg
	}

	l := &library{
		config:    *config,
		startedAt: time.Now(),
	}

	if err := l.Init(config); err != nil {
		return nil, err
	}

	return l, nil
}

func (l *library) Init(config *Config) error {
	// 初始化日志
	l.log = logger.New(l.config.Log)

	l.log.Info("sip", "initializing SIP library v%s", Version)

	// 初始化传输层
	l.tp = transport.NewTransportLayer(l.config.Transport, l.log)
	if err := l.tp.Start(); err != nil {
		return err
	}

	// 初始化事务管理器（必须在传输层消息处理器之前初始化）
	l.txMgr = transaction.NewManager(l.config.Transaction, l.tp, l.log)

	// 初始化 UAC
	l.uacInst = uac.New(l.txMgr, l.tp, l.log)

	// 初始化 UAS
	l.uasInst = uas.New(l.txMgr, l.tp, l.log)

	// 设置传输层消息处理（依赖 txMgr/uasInst，必须在它们之后设置）
	l.tp.SetMessageHandler(func(msg interface{}, src net.Addr, proto transport.Protocol) {
		// 消息路由：先匹配事务，再分发给 UAS
		switch m := msg.(type) {
		case *message.Response:
			l.txMgr.HandleResponse(m)
		case *message.Request:
			tx := l.txMgr.Find(txKeyFromRequest(m))
			if tx == nil {
				// 未匹配到事务的新请求，交给 UAS 处理
				l.uasInst.HandleRequest(m)
			} else {
				// 已有事务的请求（重传等），交给事务层处理
				l.txMgr.HandleRequest(m)
			}
		}
	})

	// 初始化 IMS
	l.regInst = ims.NewRegistrar(l.txMgr, l.tp, l.log)
	if l.config.IMS.EnableServer {
		l.imsInst = ims.NewIMSServer(l.log)
	}

	// 初始化媒体引擎
	l.mediaInst = media.NewMediaEngine(l.log)

	// 根据媒体模式初始化对应组件
	switch l.config.Media.Mode {
	case media.MediaModeRelay:
		l.relayInst = media.NewMediaRelay(l.log)
		l.log.Info("sipua", "media mode: Relay")
	case media.MediaModeTranscode:
		l.transcodeInst = media.NewMediaTranscoder(l.log)
		l.log.Info("sipua", "media mode: Transcode")
	case media.MediaModeSignalingOnly:
		l.log.Info("sipua", "media mode: Signaling Only")
	}

	// 初始化 RTP 流复制
	l.forkInst = fork.NewSession()

	// 初始化代理处理
	l.proxyInst = proxy.NewHandler(l.log)

	// 初始化头域操作器
	if l.config.Header.EnableManipulator {
		l.headerManip = headermanip.NewManipulator()
		l.log.Info("sipua", "header manipulator enabled with %d rules", len(l.config.Header.Rules))
	}

	// 初始化 CDR 管理器
	l.cdrMgr = cdr.NewManager(l.log)

	// 初始化 SIP 路由器
	l.routerInst = router.NewRouter(l.log)

	l.log.Info("sip", "SIP library initialized successfully")
	return nil
}

func (l *library) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()

	l.log.Info("sip", "shutting down SIP library...")

	// 按依赖顺序关闭
	if l.uacInst != nil {
		l.uacInst.Shutdown(ctx)
	}
	if l.uasInst != nil {
		l.uasInst.Shutdown(ctx)
	}
	if l.imsInst != nil {
		l.imsInst.Shutdown(ctx)
	}
	if l.txMgr != nil {
		l.txMgr.Shutdown(ctx)
	}
	if l.forkInst != nil {
		l.forkInst.StopAllForks()
	}
	if l.tp != nil {
		l.tp.Stop()
	}

	l.log.Info("sip", "SIP library shutdown complete")
	return nil
}

func (l *library) UAC() uac.UAC                               { return l.uacInst }
func (l *library) UAS() uas.UAS                               { return l.uasInst }
func (l *library) Registrar() ims.Registrar                   { return l.regInst }
func (l *library) IMSServer() ims.IMSServer                   { return l.imsInst }
func (l *library) Transport() transport.TransportLayer        { return l.tp }
func (l *library) Transaction() transaction.Manager           { return l.txMgr }
func (l *library) Media() media.MediaEngine                   { return l.mediaInst }
func (l *library) MediaRelay() media.MediaRelay               { return l.relayInst }
func (l *library) MediaTranscoder() media.MediaTranscoder     { return l.transcodeInst }
func (l *library) Fork() fork.Session                         { return l.forkInst }
func (l *library) Proxy() proxy.Handler                       { return l.proxyInst }
func (l *library) HeaderManipulator() headermanip.Manipulator { return l.headerManip }
func (l *library) CDR() cdr.Manager                           { return l.cdrMgr }
func (l *library) Router() router.Router                      { return l.routerInst }
func (l *library) Logger() logger.Logger                      { return l.log }
func (l *library) GetMediaMode() media.MediaMode              { return l.config.Media.Mode }

func (l *library) GetStats() *Stats {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	return &Stats{
		UAC:         l.uacInst.GetStats(),
		UAS:         l.uasInst.GetStats(),
		Transaction: l.txMgr.Stats(),
		Transport:   l.tp.Stats(),
		Proxy:       l.proxyInst.GetStats(),
		Memory: &MemoryStats{
			Alloc:        memStats.Alloc,
			TotalAlloc:   memStats.TotalAlloc,
			Sys:          memStats.Sys,
			NumGC:        memStats.NumGC,
			PauseTotalNs: memStats.PauseTotalNs,
		},
		GoroutineCount: runtime.NumGoroutine(),
		Uptime:         time.Since(l.startedAt),
	}
}

func (l *library) handleIncomingRequest(req *message.Request) {
	// 根据请求方法路由到对应的处理模块
	switch req.Method {
	case message.REGISTER:
		if l.imsInst != nil {
			l.imsInst.ProcessRegister(context.Background(), req)
		}
	default:
		// 其他请求由 UAS 模块处理
		l.uasInst.HandleRequest(req)
	}
}

// txKeyFromRequest 从请求中提取事务匹配 key。
func txKeyFromRequest(req *message.Request) string {
	vias := req.Via()
	branch := ""
	if len(vias) > 0 {
		if b, ok := vias[0].Params.Get("branch"); ok {
			branch = b
		}
	}
	sentBy := ""
	if len(vias) > 0 {
		sentBy = vias[0].SentBy()
	}
	return fmt.Sprintf("server|%s|%s|%s", req.Method, branch, sentBy)
}
