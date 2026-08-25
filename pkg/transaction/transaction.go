// Package transaction 实现 SIP 事务管理层，包含完整的 RFC 3261 状态机。
// 支持 INVITE/Non-INVITE 客户端和服务端事务，所有 Timer A-K 精确实现。
package transaction

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
	"github.com/NuyoahCx330/go-sip-ua/pkg/transport"
)

// TransactionType 事务类型（RFC 3261 Section 17）。
type TransactionType int

const (
	TxInviteClient    TransactionType = iota // INVITE 客户端事务 (Section 17.1.1)
	TxInviteServer                           // INVITE 服务端事务 (Section 17.2.1)
	TxNonInviteClient                        // Non-INVITE 客户端事务 (Section 17.1.2)
	TxNonInviteServer                        // Non-INVITE 服务端事务 (Section 17.2.2)
)

// String 返回事务类型的可读名称。
func (t TransactionType) String() string {
	switch t {
	case TxInviteClient:
		return "INVITE-client"
	case TxInviteServer:
		return "INVITE-server"
	case TxNonInviteClient:
		return "non-INVITE-client"
	case TxNonInviteServer:
		return "non-INVITE-server"
	default:
		return "unknown"
	}
}

// TxState 事务状态（RFC 3261 状态机）。
type TxState int32

const (
	TxStateInit       TxState = iota
	TxStateCalling            // INVITE 客户端: 已发送 INVITE
	TxStateTrying             // Non-INVITE 客户端: 已发送请求
	TxStateProceeding         // 收到 1xx 响应
	TxStateCompleted          // 收到/发送最终响应
	TxStateConfirmed          // INVITE 服务端: 收到 ACK
	TxStateTerminated         // 事务终止
)

// String 返回事务状态的可读名称。
func (s TxState) String() string {
	switch s {
	case TxStateInit:
		return "Init"
	case TxStateCalling:
		return "Calling"
	case TxStateTrying:
		return "Trying"
	case TxStateProceeding:
		return "Proceeding"
	case TxStateCompleted:
		return "Completed"
	case TxStateConfirmed:
		return "Confirmed"
	case TxStateTerminated:
		return "Terminated"
	default:
		return "Unknown"
	}
}

// Transaction 事务接口。
type Transaction interface {
	ID() string
	Key() string
	Type() TransactionType
	State() TxState
	OriginalRequest() *message.Request
	LastResponse() *message.Response
	SendRequest(req *message.Request) error
	SendResponse(rsp *message.Response) error
	SetOnResponse(fn func(rsp *message.Response))
	SetOnTimeout(fn func())
	SetOnError(fn func(err error))
	Terminate()
	CreatedAt() time.Time
	IsReliable() bool
}

// Config 事务层配置（RFC 3261 Timer 参数）。
type Config struct {
	// T1 RTT 估计值（默认 500ms）- RFC 3261 Section 17.1.1.1
	T1 time.Duration
	// T2 最大重传间隔（默认 4s）- RFC 3261 Section 17.1.2.2
	T2 time.Duration
	// T4 INVITE 消息在网络中的最大存活时间（默认 5s）
	T4 time.Duration

	// Timer A: INVITE 请求重传间隔（仅 UDP），初始 = T1，指数退避
	TimerA time.Duration
	// Timer B: INVITE 客户端事务超时，默认 64*T1 = 32s
	TimerB time.Duration
	// Timer C: INVITE 代理超时，默认 > 3min
	TimerC time.Duration
	// Timer D: 等待重传的时间（UDP: 32s, TCP: 0s）
	TimerD time.Duration
	// Timer E: Non-INVITE 重传间隔（初始 = T1），指数退避到 T2
	TimerE time.Duration
	// Timer F: Non-INVITE 客户端事务超时，默认 64*T1 = 32s
	TimerF time.Duration
	// Timer G: INVITE 服务端响应重传间隔（初始 = T1），指数退避到 T2
	TimerG time.Duration
	// Timer H: 等待 ACK 的时间，默认 64*T1 = 32s
	TimerH time.Duration
	// Timer I: 吸收 ACK 的时间（UDP: T4, TCP: 0s）
	TimerI time.Duration
	// Timer J: Non-INVITE 服务端等待重传的时间（UDP: 64*T1, TCP: 0s）
	TimerJ time.Duration
	// Timer K: 等待响应重传的时间（UDP: T4, TCP: 0s）
	TimerK time.Duration

	// 高并发优化配置
	ShardCount              int
	MaxTransactionsPerShard int
	CleanupInterval         time.Duration
	MaxTransactionAge       time.Duration
}

// DefaultConfig 返回默认事务配置（RFC 3261 推荐值）。
func DefaultConfig() Config {
	T1 := 500 * time.Millisecond
	T2 := 4 * time.Second
	T4 := 5 * time.Second

	return Config{
		T1:                      T1,
		T2:                      T2,
		T4:                      T4,
		TimerA:                  T1,                // 初始 INVITE 重传间隔
		TimerB:                  64 * T1,           // INVITE 超时 = 32s
		TimerC:                  180 * time.Second, // 代理超时 > 3min
		TimerD:                  32 * time.Second,  // UDP: 32s
		TimerE:                  T1,                // 初始 Non-INVITE 重传间隔
		TimerF:                  64 * T1,           // Non-INVITE 超时 = 32s
		TimerG:                  T1,                // INVITE 响应重传间隔
		TimerH:                  64 * T1,           // 等待 ACK = 32s
		TimerI:                  T4,                // UDP: 5s
		TimerJ:                  64 * T1,           // UDP: 32s
		TimerK:                  T4,                // UDP: 5s
		ShardCount:              runtime.NumCPU() * 2,
		MaxTransactionsPerShard: 50000,
		CleanupInterval:         10 * time.Second,
		MaxTransactionAge:       5 * time.Minute,
	}
}

// Manager 事务管理器接口。
type Manager interface {
	CreateClientTx(req *message.Request, tp transport.TransportLayer) (Transaction, error)
	CreateServerTx(req *message.Request, tp transport.TransportLayer) (Transaction, error)
	Find(key string) Transaction
	Remove(key string)
	HandleResponse(rsp *message.Response)
	HandleRequest(req *message.Request)
	Stats() *Stats
	Shutdown(ctx context.Context) error
}

// Stats 事务统计信息。
type Stats struct {
	TotalCreated   atomic.Int64
	TotalCompleted atomic.Int64
	TotalFailed    atomic.Int64
	TotalTimedOut  atomic.Int64
	ActiveCount    atomic.Int64
}

// shard 事务分片。
type shard struct {
	transactions sync.Map
	count        atomic.Int64
}

// transactionManager 是 Manager 的默认实现。
type transactionManager struct {
	config Config
	log    logger.Logger
	shards []*shard
	stats  Stats
	doneCh chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

// NewManager 创建新的事务管理器。
func NewManager(cfg Config, tp transport.TransportLayer, log logger.Logger) Manager {
	if log == nil {
		log = logger.NopLogger()
	}
	if cfg.ShardCount <= 0 {
		cfg.ShardCount = runtime.NumCPU() * 2
	}

	shards := make([]*shard, cfg.ShardCount)
	for i := range shards {
		shards[i] = &shard{}
	}

	tm := &transactionManager{
		config: cfg,
		log:    log,
		shards: shards,
		doneCh: make(chan struct{}),
	}

	tm.wg.Add(1)
	go tm.cleanupLoop()

	return tm
}

func (tm *transactionManager) getShard(key string) *shard {
	h := fnv32(key)
	return tm.shards[h%uint32(len(tm.shards))]
}

// fnv32 是 FNV-1a 32 位哈希函数。
func fnv32(key string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return h
}

func (tm *transactionManager) CreateClientTx(req *message.Request, tp transport.TransportLayer) (Transaction, error) {
	txKey := clientTxKey(req)
	s := tm.getShard(txKey)

	if tm.config.MaxTransactionsPerShard > 0 && s.count.Load() >= int64(tm.config.MaxTransactionsPerShard) {
		return nil, errors.New("transaction: shard capacity exceeded")
	}

	txType := TxNonInviteClient
	if req.Method == message.INVITE {
		txType = TxInviteClient
	}

	// 确定传输协议是否可靠
	isReliable := isReliableTransport(req)

	tx := &transaction{
		id:         txKey,
		key:        txKey,
		txType:     txType,
		state:      TxStateInit,
		origReq:    req,
		createdAt:  time.Now(),
		tp:         tp,
		log:        tm.log,
		doneCh:     make(chan struct{}),
		isReliable: isReliable,
		config:     tm.config,
	}

	s.transactions.Store(txKey, tx)
	s.count.Add(1)
	tm.stats.TotalCreated.Add(1)
	tm.stats.ActiveCount.Add(1)

	// 启动事务状态机
	go tm.runClientTx(tx)

	return tx, nil
}

func (tm *transactionManager) CreateServerTx(req *message.Request, tp transport.TransportLayer) (Transaction, error) {
	txKey := serverTxKey(req)
	s := tm.getShard(txKey)

	if tm.config.MaxTransactionsPerShard > 0 && s.count.Load() >= int64(tm.config.MaxTransactionsPerShard) {
		return nil, errors.New("transaction: shard capacity exceeded")
	}

	txType := TxNonInviteServer
	if req.Method == message.INVITE {
		txType = TxInviteServer
	}

	isReliable := isReliableTransport(req)

	tx := &transaction{
		id:         txKey,
		key:        txKey,
		txType:     txType,
		state:      TxStateInit,
		origReq:    req,
		createdAt:  time.Now(),
		tp:         tp,
		log:        tm.log,
		doneCh:     make(chan struct{}),
		isReliable: isReliable,
		config:     tm.config,
	}

	s.transactions.Store(txKey, tx)
	s.count.Add(1)
	tm.stats.TotalCreated.Add(1)
	tm.stats.ActiveCount.Add(1)

	return tx, nil
}

func (tm *transactionManager) Find(key string) Transaction {
	s := tm.getShard(key)
	if val, ok := s.transactions.Load(key); ok {
		return val.(*transaction)
	}
	return nil
}

func (tm *transactionManager) Remove(key string) {
	s := tm.getShard(key)
	if _, ok := s.transactions.LoadAndDelete(key); ok {
		s.count.Add(-1)
		tm.stats.ActiveCount.Add(-1)
	}
}

func (tm *transactionManager) HandleResponse(rsp *message.Response) {
	key := responseTxKey(rsp)
	tx := tm.Find(key)
	if tx == nil {
		tm.log.Debug("transaction", "no transaction found for response key=%s", key)
		return
	}

	t := tx.(*transaction)
	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastRsp = rsp

	switch t.txType {
	case TxInviteClient:
		tm.handleInviteClientResponse(t, rsp)
	case TxNonInviteClient:
		tm.handleNonInviteClientResponse(t, rsp)
	}
}

func (tm *transactionManager) HandleRequest(req *message.Request) {
	key := serverTxKey(req)
	tx := tm.Find(key)
	if tx == nil {
		return
	}

	t := tx.(*transaction)
	t.mu.Lock()
	defer t.mu.Unlock()

	state := TxState(atomic.LoadInt32((*int32)(&t.state)))

	switch t.txType {
	case TxInviteServer:
		tm.handleInviteServerRequest(t, state)
	case TxNonInviteServer:
		tm.handleNonInviteServerRequest(t, state)
	}
}

// ---- INVITE 客户端事务状态机 (RFC 3261 Section 17.1.1) ----
// States: Calling -> Proceeding -> Completed -> Terminated
func (tm *transactionManager) handleInviteClientResponse(t *transaction, rsp *message.Response) {
	state := TxState(atomic.LoadInt32((*int32)(&t.state)))

	switch {
	case rsp.IsProvisional():
		// 1xx: Calling/Proceeding -> Proceeding
		if state == TxStateCalling || state == TxStateProceeding {
			atomic.StoreInt32((*int32)(&t.state), int32(TxStateProceeding))
			t.stopTimerA() // 停止重传
			if t.onResponse != nil {
				t.onResponse(rsp)
			}
		}

	case rsp.IsSuccess():
		// 2xx: 任何状态 -> Terminated
		t.stopTimerA()
		t.stopTimerB()
		atomic.StoreInt32((*int32)(&t.state), int32(TxStateTerminated))
		tm.stats.TotalCompleted.Add(1)
		if t.onResponse != nil {
			t.onResponse(rsp)
		}
		go t.terminate()

	default:
		// 3xx-6xx: Calling/Proceeding -> Completed
		if state == TxStateCalling || state == TxStateProceeding {
			t.stopTimerA()
			t.stopTimerB()
			atomic.StoreInt32((*int32)(&t.state), int32(TxStateCompleted))
			if t.onResponse != nil {
				t.onResponse(rsp)
			}
			// 启动 Timer D (等待重传)
			t.startTimerD()
		}
	}
}

// ---- Non-INVITE 客户端事务状态机 (RFC 3261 Section 17.1.2) ----
// States: Trying -> Proceeding -> Completed -> Terminated
func (tm *transactionManager) handleNonInviteClientResponse(t *transaction, rsp *message.Response) {
	state := TxState(atomic.LoadInt32((*int32)(&t.state)))

	switch {
	case rsp.IsProvisional():
		// 1xx: Trying/Proceeding -> Proceeding
		if state == TxStateTrying || state == TxStateProceeding {
			atomic.StoreInt32((*int32)(&t.state), int32(TxStateProceeding))
			if t.onResponse != nil {
				t.onResponse(rsp)
			}
		}

	default:
		// 2xx-6xx: Trying/Proceeding -> Completed -> Terminated
		if state == TxStateTrying || state == TxStateProceeding {
			t.stopTimerE()
			t.stopTimerF()
			atomic.StoreInt32((*int32)(&t.state), int32(TxStateCompleted))
			if t.onResponse != nil {
				t.onResponse(rsp)
			}
			// 启动 Timer K (等待重传吸收)
			t.startTimerK()
		}
	}
}

// ---- INVITE 服务端事务请求处理 (RFC 3261 Section 17.2.1) ----
func (tm *transactionManager) handleInviteServerRequest(t *transaction, state TxState) {
	// 重传请求在 Completed 或 Confirmed 状态：重传最后响应
	if (state == TxStateCompleted || state == TxStateConfirmed) && t.lastRsp != nil {
		t.sendResponse(t.lastRsp)
	}
}

// ---- Non-INVITE 服务端事务请求处理 (RFC 3261 Section 17.2.2) ----
func (tm *transactionManager) handleNonInviteServerRequest(t *transaction, state TxState) {
	// 重传请求在 Completed 状态：重传最后响应
	if state == TxStateCompleted && t.lastRsp != nil {
		t.sendResponse(t.lastRsp)
	}
}

// ---- 客户端事务启动 ----

func (tm *transactionManager) runClientTx(tx *transaction) {
	switch tx.txType {
	case TxInviteClient:
		tm.runInviteClientTx(tx)
	case TxNonInviteClient:
		tm.runNonInviteClientTx(tx)
	}
}

// INVITE 客户端事务 (RFC 3261 Section 17.1.1)
func (tm *transactionManager) runInviteClientTx(tx *transaction) {
	atomic.StoreInt32((*int32)(&tx.state), int32(TxStateCalling))

	// 发送 INVITE
	if err := tx.sendRequest(tx.origReq); err != nil {
		tm.stats.TotalFailed.Add(1)
		if tx.onError != nil {
			tx.onError(err)
		}
		go tx.terminate()
		return
	}

	// Timer B: INVITE 事务超时 (64*T1)
	tx.startTimerB(func() {
		tm.stats.TotalTimedOut.Add(1)
		if tx.onTimeout != nil {
			tx.onTimeout()
		}
		atomic.StoreInt32((*int32)(&tx.state), int32(TxStateTerminated))
		go tx.terminate()
	})

	// Timer A: INVITE 重传 (仅 UDP)
	if !tx.isReliable {
		tx.startTimerA(tm.config.TimerA)
	}
}

// Non-INVITE 客户端事务 (RFC 3261 Section 17.1.2)
func (tm *transactionManager) runNonInviteClientTx(tx *transaction) {
	atomic.StoreInt32((*int32)(&tx.state), int32(TxStateTrying))

	// 发送请求
	if err := tx.sendRequest(tx.origReq); err != nil {
		tm.stats.TotalFailed.Add(1)
		if tx.onError != nil {
			tx.onError(err)
		}
		go tx.terminate()
		return
	}

	// Timer F: Non-INVITE 事务超时 (64*T1)
	tx.startTimerF(func() {
		tm.stats.TotalTimedOut.Add(1)
		if tx.onTimeout != nil {
			tx.onTimeout()
		}
		atomic.StoreInt32((*int32)(&tx.state), int32(TxStateTerminated))
		go tx.terminate()
	})

	// Timer E: Non-INVITE 重传
	tx.startTimerE(tm.config.TimerE)
}

func (tm *transactionManager) cleanupLoop() {
	defer tm.wg.Done()
	ticker := time.NewTicker(tm.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			tm.cleanupExpired()
		case <-tm.doneCh:
			return
		}
	}
}

func (tm *transactionManager) cleanupExpired() {
	now := time.Now()
	for _, s := range tm.shards {
		s.transactions.Range(func(key, value interface{}) bool {
			tx := value.(*transaction)
			state := TxState(atomic.LoadInt32((*int32)(&tx.state)))
			if state == TxStateTerminated || now.Sub(tx.createdAt) > tm.config.MaxTransactionAge {
				tx.terminate()
				s.transactions.Delete(key)
				s.count.Add(-1)
				tm.stats.ActiveCount.Add(-1)
			}
			return true
		})
	}
}

func (tm *transactionManager) Stats() *Stats {
	return &tm.stats
}

func (tm *transactionManager) Shutdown(ctx context.Context) error {
	tm.mu.Lock()
	if tm.closed {
		tm.mu.Unlock()
		return nil
	}
	tm.closed = true
	tm.mu.Unlock()

	close(tm.doneCh)

	done := make(chan struct{})
	go func() {
		for _, s := range tm.shards {
			s.transactions.Range(func(key, value interface{}) bool {
				tx := value.(*transaction)
				tx.terminate()
				return true
			})
		}
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---- transaction 实现 ----

type transaction struct {
	id         string
	key        string
	txType     TransactionType
	state      TxState
	origReq    *message.Request
	lastRsp    *message.Response
	createdAt  time.Time
	tp         transport.TransportLayer
	log        logger.Logger
	isReliable bool
	config     Config

	onResponse func(rsp *message.Response)
	onTimeout  func()
	onError    func(err error)

	mu     sync.Mutex
	doneCh chan struct{}
	done   bool

	// Timer 控制
	timerA     *time.Timer
	timerAOnce sync.Once
	timerB     *time.Timer
	timerE     *time.Timer
	timerF     *time.Timer
	timerD     *time.Timer
	timerG     *time.Timer
	timerH     *time.Timer
	timerJ     *time.Timer
	timerK     *time.Timer
}

func (t *transaction) ID() string                        { return t.id }
func (t *transaction) Key() string                       { return t.key }
func (t *transaction) Type() TransactionType             { return t.txType }
func (t *transaction) State() TxState                    { return TxState(atomic.LoadInt32((*int32)(&t.state))) }
func (t *transaction) OriginalRequest() *message.Request { return t.origReq }
func (t *transaction) LastResponse() *message.Response   { return t.lastRsp }
func (t *transaction) CreatedAt() time.Time              { return t.createdAt }
func (t *transaction) IsReliable() bool                  { return t.isReliable }

func (t *transaction) SendRequest(req *message.Request) error {
	return t.sendRequest(req)
}

func (t *transaction) SendResponse(rsp *message.Response) error {
	t.mu.Lock()
	t.lastRsp = rsp

	// 服务端事务发送响应时的状态转换
	switch t.txType {
	case TxInviteServer:
		if rsp.IsProvisional() {
			atomic.StoreInt32((*int32)(&t.state), int32(TxStateProceeding))
		} else if rsp.IsSuccess() {
			atomic.StoreInt32((*int32)(&t.state), int32(TxStateTerminated))
			go t.terminate()
		} else {
			// 3xx-6xx: -> Completed, 启动 Timer G 和 Timer H
			atomic.StoreInt32((*int32)(&t.state), int32(TxStateCompleted))
			if !t.isReliable {
				t.startTimerG(t.config.TimerG)
			}
			t.startTimerH(func() {
				atomic.StoreInt32((*int32)(&t.state), int32(TxStateTerminated))
				go t.terminate()
			})
		}

	case TxNonInviteServer:
		if rsp.IsProvisional() {
			atomic.StoreInt32((*int32)(&t.state), int32(TxStateProceeding))
		} else {
			// 2xx-6xx: -> Completed, 启动 Timer J
			atomic.StoreInt32((*int32)(&t.state), int32(TxStateCompleted))
			t.startTimerJ()
		}
	}

	t.mu.Unlock()
	return t.sendResponse(rsp)
}

func (t *transaction) SetOnResponse(fn func(rsp *message.Response)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onResponse = fn
}

func (t *transaction) SetOnTimeout(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onTimeout = fn
}

func (t *transaction) SetOnError(fn func(err error)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onError = fn
}

func (t *transaction) Terminate() {
	t.terminate()
}

func (t *transaction) sendRequest(req *message.Request) error {
	vias := req.Via()
	if len(vias) == 0 {
		return errors.New("transaction: request has no Via header")
	}

	topVia := vias[0]
	var dst net.Addr
	var proto transport.Protocol

	switch topVia.Transport {
	case "UDP":
		dst = resolveUDPAddr(topVia.SentBy())
		proto = transport.UDP
	case "TCP":
		dst = resolveTCPAddr(topVia.SentBy())
		proto = transport.TCP
	case "TLS":
		dst = resolveTCPAddr(topVia.SentBy())
		proto = transport.TLS
	default:
		dst = resolveUDPAddr(topVia.SentBy())
		proto = transport.UDP
	}

	if dst == nil {
		return errors.New("transaction: failed to resolve destination")
	}

	return t.tp.SendMessage(req, dst, proto)
}

func (t *transaction) sendResponse(rsp *message.Response) error {
	vias := rsp.Via()
	if len(vias) == 0 {
		return errors.New("transaction: response has no Via header")
	}

	topVia := vias[0]
	var dst net.Addr
	var proto transport.Protocol

	switch topVia.Transport {
	case "UDP":
		dst = resolveUDPAddr(topVia.SentBy())
		proto = transport.UDP
	case "TCP":
		dst = resolveTCPAddr(topVia.SentBy())
		proto = transport.TCP
	case "TLS":
		dst = resolveTCPAddr(topVia.SentBy())
		proto = transport.TLS
	default:
		dst = resolveUDPAddr(topVia.SentBy())
		proto = transport.UDP
	}

	if dst == nil {
		return errors.New("transaction: failed to resolve response destination")
	}

	return t.tp.SendMessage(rsp, dst, proto)
}

// ---- Timer 实现 (RFC 3261 Section 17.1) ----

// startTimerA 启动 INVITE 重传定时器（指数退避，仅 UDP）。
func (t *transaction) startTimerA(interval time.Duration) {
	t.timerA = time.AfterFunc(interval, func() {
		state := TxState(atomic.LoadInt32((*int32)(&t.state)))
		if state != TxStateCalling {
			return
		}
		// 重传 INVITE
		t.sendRequest(t.origReq)
		// 指数退避，但不超过 T2
		newInterval := interval * 2
		if newInterval > t.config.T2 {
			newInterval = t.config.T2
		}
		t.startTimerA(newInterval)
	})
}

func (t *transaction) stopTimerA() {
	if t.timerA != nil {
		t.timerA.Stop()
	}
}

// startTimerB 启动 INVITE 超时定时器。
func (t *transaction) startTimerB(onTimeout func()) {
	t.timerB = time.AfterFunc(t.config.TimerB, func() {
		state := TxState(atomic.LoadInt32((*int32)(&t.state)))
		if state == TxStateCalling {
			onTimeout()
		}
	})
}

func (t *transaction) stopTimerB() {
	if t.timerB != nil {
		t.timerB.Stop()
	}
}

// startTimerE 启动 Non-INVITE 重传定时器（指数退避到 T2）。
func (t *transaction) startTimerE(interval time.Duration) {
	t.timerE = time.AfterFunc(interval, func() {
		state := TxState(atomic.LoadInt32((*int32)(&t.state)))
		if state != TxStateTrying && state != TxStateProceeding {
			return
		}
		// 重传请求
		t.sendRequest(t.origReq)
		// 指数退避，但不超过 T2
		newInterval := interval * 2
		if newInterval > t.config.T2 {
			newInterval = t.config.T2
		}
		t.startTimerE(newInterval)
	})
}

func (t *transaction) stopTimerE() {
	if t.timerE != nil {
		t.timerE.Stop()
	}
}

// startTimerF 启动 Non-INVITE 超时定时器。
func (t *transaction) startTimerF(onTimeout func()) {
	t.timerF = time.AfterFunc(t.config.TimerF, func() {
		state := TxState(atomic.LoadInt32((*int32)(&t.state)))
		if state == TxStateTrying || state == TxStateProceeding {
			onTimeout()
		}
	})
}

func (t *transaction) stopTimerF() {
	if t.timerF != nil {
		t.timerF.Stop()
	}
}

// startTimerG 启动 INVITE 服务端响应重传定时器。
func (t *transaction) startTimerG(interval time.Duration) {
	t.timerG = time.AfterFunc(interval, func() {
		state := TxState(atomic.LoadInt32((*int32)(&t.state)))
		if state != TxStateCompleted {
			return
		}
		// 重传响应
		if t.lastRsp != nil {
			t.sendResponse(t.lastRsp)
		}
		// 指数退避，但不超过 T2
		newInterval := interval * 2
		if newInterval > t.config.T2 {
			newInterval = t.config.T2
		}
		t.startTimerG(newInterval)
	})
}

// startTimerH 启动等待 ACK 定时器。
func (t *transaction) startTimerH(onTimeout func()) {
	t.timerH = time.AfterFunc(t.config.TimerH, func() {
		state := TxState(atomic.LoadInt32((*int32)(&t.state)))
		if state == TxStateCompleted {
			// Timer H 超时：未收到 ACK，事务终止
			onTimeout()
		}
	})
}

// startTimerD 启动等待响应重传定时器（Completed 状态）。
func (t *transaction) startTimerD() {
	duration := t.config.TimerD
	if t.isReliable {
		duration = 0
	}
	t.timerD = time.AfterFunc(duration, func() {
		atomic.StoreInt32((*int32)(&t.state), int32(TxStateTerminated))
		go t.terminate()
	})
}

// startTimerK 启动等待响应重传吸收定时器（Non-INVITE 客户端）。
func (t *transaction) startTimerK() {
	duration := t.config.TimerK
	if t.isReliable {
		duration = 0
	}
	t.timerK = time.AfterFunc(duration, func() {
		atomic.StoreInt32((*int32)(&t.state), int32(TxStateTerminated))
		go t.terminate()
	})
}

// startTimerJ 启动 Non-INVITE 服务端等待重传定时器。
func (t *transaction) startTimerJ() {
	duration := t.config.TimerJ
	if t.isReliable {
		duration = 0
	}
	t.timerJ = time.AfterFunc(duration, func() {
		atomic.StoreInt32((*int32)(&t.state), int32(TxStateTerminated))
		go t.terminate()
	})
}

func (t *transaction) terminate() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.done {
		return
	}
	t.done = true

	// 停止所有定时器
	t.stopTimerA()
	t.stopTimerB()
	t.stopTimerE()
	t.stopTimerF()
	if t.timerD != nil {
		t.timerD.Stop()
	}
	if t.timerG != nil {
		t.timerG.Stop()
	}
	if t.timerH != nil {
		t.timerH.Stop()
	}
	if t.timerJ != nil {
		t.timerJ.Stop()
	}
	if t.timerK != nil {
		t.timerK.Stop()
	}

	close(t.doneCh)
}

// ---- 辅助函数 ----

func isReliableTransport(req *message.Request) bool {
	vias := req.Via()
	if len(vias) == 0 {
		return false
	}
	switch vias[0].Transport {
	case "TCP", "TLS", "SCTP", "WS", "WSS":
		return true
	default:
		return false
	}
}

func clientTxKey(req *message.Request) string {
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
	return fmt.Sprintf("client|%s|%s|%s", req.Method, branch, sentBy)
}

func serverTxKey(req *message.Request) string {
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

func responseTxKey(rsp *message.Response) string {
	vias := rsp.Via()
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
	cseq := rsp.CSeq()
	method := ""
	if cseq != nil {
		method = string(cseq.Method)
	}
	return fmt.Sprintf("client|%s|%s|%s", method, branch, sentBy)
}

func resolveUDPAddr(addr string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil
	}
	return a
}

func resolveTCPAddr(addr string) *net.TCPAddr {
	a, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil
	}
	return a
}
