// Package uac 实现 UAC（用户代理客户端），负责发起外呼、管理呼叫状态和处理会话中请求。
package uac

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/media"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
	"github.com/NuyoahCx330/go-sip-ua/pkg/transaction"
	"github.com/NuyoahCx330/go-sip-ua/pkg/transport"
)

// CallHandle 呼叫句柄，使用 uint64 提升性能。
type CallHandle uint64

// CallState 呼叫状态。
type CallState int32

const (
	CallStateIdle         CallState = iota
	CallStateCalling                // 已发送 INVITE
	CallStateProceeding             // 收到 1xx
	CallStateEarly                  // 收到 18x（早期媒体）
	CallStateConnected              // 收到 200 OK
	CallStateDisconnected           // 收到 BYE
	CallStateTerminated             // 呼叫终止
)

// String 返回呼叫状态的可读名称。
func (s CallState) String() string {
	switch s {
	case CallStateIdle:
		return "Idle"
	case CallStateCalling:
		return "Calling"
	case CallStateProceeding:
		return "Proceeding"
	case CallStateEarly:
		return "Early"
	case CallStateConnected:
		return "Connected"
	case CallStateDisconnected:
		return "Disconnected"
	case CallStateTerminated:
		return "Terminated"
	default:
		return "Unknown"
	}
}

// CallParam 外呼参数。
type CallParam struct {
	FromURI              *message.URI
	ToURI                *message.URI
	SDP                  []byte
	Contact              *message.URI
	DisplayName          string
	UserAgent            string
	Supported            []string
	Expires              int
	SessionTimer         bool
	EarlyMedia           bool
	CallingPartyCategory string
	ExtraHeaders         map[string]string
	MaxForwards          int
	Priority             int
	Timeout              time.Duration
}

// ReferOptions REFER 转接选项。
type ReferOptions struct {
	ReferTo    string
	ReferredBy string
	Expires    int
}

// DTMFOptions DTMF 发送选项。
type DTMFOptions struct {
	Method   string // "RFC2833" 或 "INFO"
	Duration int    // 毫秒
}

// DialogInfo 对话信息。
type DialogInfo struct {
	CallID    string
	LocalTag  string
	RemoteTag string
	LocalURI  *message.URI
	RemoteURI *message.URI
	LocalSeq  uint32
	RemoteSeq uint32
	RouteSet  []*message.URI
	Secure    bool
}

// Call 呼叫接口。
type Call interface {
	ID() CallHandle
	Dialog() *DialogInfo
	State() CallState
	RemoteURI() *message.URI
	LocalURI() *message.URI
	GetSDP() []byte
	SetUserData(data interface{})
	UserData() interface{}
	CreatedAt() time.Time
	Duration() time.Duration
}

// Callback UAC 事件回调接口。
type Callback interface {
	OnCallStateChange(handle CallHandle, state CallState, oldState CallState)
	OnIncomingResponse(handle CallHandle, rsp *message.Response)
	OnIncomingRequest(handle CallHandle, req *message.Request) error
	OnMediaUpdate(handle CallHandle, sdp []byte)
	OnError(handle CallHandle, err error)
}

// Stats UAC 统计信息。
type Stats struct {
	TotalCalls       atomic.Int64
	ActiveCalls      atomic.Int64
	CompletedCalls   atomic.Int64
	FailedCalls      atomic.Int64
	AverageSetupTime atomic.Int64 // 纳秒
	P99SetupTime     atomic.Int64 // 纳秒
	setupTimes       []int64      // 用于计算 P99
	setupTimesMu     sync.Mutex
}

// UAC 用户代理客户端接口。
type UAC interface {
	OutgoingCall(ctx context.Context, param *CallParam) (CallHandle, error)
	TerminateCall(ctx context.Context, handle CallHandle, reason string) error
	HoldCall(ctx context.Context, handle CallHandle, hold bool) error
	ReferCall(ctx context.Context, handle CallHandle, referTo string, opts *ReferOptions) error
	SendDTMF(ctx context.Context, handle CallHandle, digit rune, opts *DTMFOptions) error
	UpdateMedia(ctx context.Context, handle CallHandle, sdp []byte) error
	GetCall(handle CallHandle) (Call, error)
	SetCallback(cb Callback) error
	GetStats() *Stats
	Shutdown(ctx context.Context) error
}

// call 是 Call 接口的实现。
type call struct {
	handle     CallHandle
	dialog     *DialogInfo
	state      atomic.Int32
	localSDP   []byte
	remoteSDP  []byte
	localURI   *message.URI
	remoteURI  *message.URI
	createdAt  time.Time
	answeredAt time.Time
	userData   interface{}
	mu         sync.RWMutex
}

func (c *call) ID() CallHandle          { return c.handle }
func (c *call) Dialog() *DialogInfo     { return c.dialog }
func (c *call) State() CallState        { return CallState(c.state.Load()) }
func (c *call) RemoteURI() *message.URI { return c.remoteURI }
func (c *call) LocalURI() *message.URI  { return c.localURI }
func (c *call) GetSDP() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.localSDP
}
func (c *call) SetUserData(data interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userData = data
}
func (c *call) UserData() interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.userData
}
func (c *call) CreatedAt() time.Time { return c.createdAt }
func (c *call) Duration() time.Duration {
	if c.answeredAt.IsZero() {
		return 0
	}
	return time.Since(c.answeredAt)
}

// uac UAC 接口的默认实现。
type uac struct {
	txMgr  transaction.Manager
	tp     transport.TransportLayer
	log    logger.Logger
	cb     Callback
	calls  sync.Map // map[CallHandle]*call
	nextID atomic.Uint64
	stats  Stats
	doneCh chan struct{}
	mu     sync.RWMutex
	closed bool
}

// New 创建 UAC 实例。
func New(txMgr transaction.Manager, tp transport.TransportLayer, log logger.Logger) UAC {
	if log == nil {
		log = logger.NopLogger()
	}
	return &uac{
		txMgr:  txMgr,
		tp:     tp,
		log:    log,
		doneCh: make(chan struct{}),
	}
}

func (u *uac) OutgoingCall(ctx context.Context, param *CallParam) (CallHandle, error) {
	u.mu.RLock()
	if u.closed {
		u.mu.RUnlock()
		return 0, errors.New("uac: shutdown")
	}
	u.mu.RUnlock()

	if param.FromURI == nil || param.ToURI == nil {
		return 0, errors.New("uac: FromURI and ToURI are required")
	}

	handle := CallHandle(u.nextID.Add(1))
	callID := message.GenerateCallID()
	localTag := message.GenerateTag()

	c := &call{
		handle: handle,
		dialog: &DialogInfo{
			CallID:    callID,
			LocalTag:  localTag,
			LocalURI:  param.FromURI,
			RemoteURI: param.ToURI,
		},
		localSDP:  param.SDP,
		localURI:  param.FromURI,
		remoteURI: param.ToURI,
		createdAt: time.Now(),
	}
	c.state.Store(int32(CallStateIdle))

	u.calls.Store(handle, c)
	u.stats.TotalCalls.Add(1)
	u.stats.ActiveCalls.Add(1)

	// 构造 INVITE 请求
	req := u.buildInvite(callID, localTag, param)

	// 创建客户端事务
	tx, err := u.txMgr.CreateClientTx(req, u.tp)
	if err != nil {
		u.calls.Delete(handle)
		u.stats.ActiveCalls.Add(-1)
		return 0, fmt.Errorf("uac: create transaction: %w", err)
	}

	// 设置响应回调
	tx.SetOnResponse(func(rsp *message.Response) {
		u.handleResponse(handle, rsp)
	})

	tx.SetOnTimeout(func() {
		u.log.Warn("uac", "call %d timed out", handle)
		u.setState(c, CallStateTerminated)
		u.stats.FailedCalls.Add(1)
		u.stats.ActiveCalls.Add(-1)
		if u.cb != nil {
			u.cb.OnError(handle, errors.New("uac: call timed out"))
		}
	})

	u.setState(c, CallStateCalling)
	u.log.Info("uac", "outgoing call %d to %s (Call-ID: %s)", handle, param.ToURI, callID)

	return handle, nil
}

func (u *uac) TerminateCall(ctx context.Context, handle CallHandle, reason string) error {
	c, err := u.getCall(handle)
	if err != nil {
		return err
	}

	state := CallState(c.state.Load())
	if state == CallStateTerminated || state == CallStateDisconnected {
		return nil
	}

	// 如果已接通，发送 BYE
	if state == CallStateConnected {
		bye := u.buildBye(c, reason)
		tx, err := u.txMgr.CreateClientTx(bye, u.tp)
		if err != nil {
			return fmt.Errorf("uac: create BYE transaction: %w", err)
		}
		_ = tx // BYE 事务在收到响应后自动终止
	} else {
		// 未接通时发送 CANCEL
		cancel := u.buildCancel(c)
		tx, err := u.txMgr.CreateClientTx(cancel, u.tp)
		if err != nil {
			return fmt.Errorf("uac: create CANCEL transaction: %w", err)
		}
		_ = tx
	}

	u.setState(c, CallStateTerminated)
	u.stats.ActiveCalls.Add(-1)
	u.stats.CompletedCalls.Add(1)
	u.log.Info("uac", "call %d terminated: %s", handle, reason)
	return nil
}

func (u *uac) HoldCall(ctx context.Context, handle CallHandle, hold bool) error {
	c, err := u.getCall(handle)
	if err != nil {
		return err
	}

	// 构造 re-INVITE（hold 时 SDP 中 audio 的 addr 设为 0.0.0.0）
	reinvite := u.buildReInvite(c)
	if hold {
		// 修改 SDP 中连接地址为 0.0.0.0 表示保持
		reinvite.Headers.Set("Session-State", "hold")
	}

	tx, err := u.txMgr.CreateClientTx(reinvite, u.tp)
	if err != nil {
		return fmt.Errorf("uac: create re-INVITE: %w", err)
	}
	_ = tx
	u.log.Info("uac", "call %d hold=%v", handle, hold)
	return nil
}

func (u *uac) ReferCall(ctx context.Context, handle CallHandle, referTo string, opts *ReferOptions) error {
	c, err := u.getCall(handle)
	if err != nil {
		return err
	}

	refer := u.buildRefer(c, referTo, opts)
	tx, err := u.txMgr.CreateClientTx(refer, u.tp)
	if err != nil {
		return fmt.Errorf("uac: create REFER: %w", err)
	}
	_ = tx
	u.log.Info("uac", "call %d REFER to %s", handle, referTo)
	return nil
}

func (u *uac) SendDTMF(ctx context.Context, handle CallHandle, digit rune, opts *DTMFOptions) error {
	c, err := u.getCall(handle)
	if err != nil {
		return err
	}

	state := CallState(c.state.Load())
	if state != CallStateConnected {
		return fmt.Errorf("uac: call %d not in connected state (state: %s)", handle, state)
	}

	method := "INFO"
	duration := 160 // 默认 20ms
	if opts != nil {
		if opts.Method == "RFC2833" {
			method = "RFC2833"
		}
		if opts.Duration > 0 {
			duration = opts.Duration
		}
	}

	switch method {
	case "RFC2833":
		return u.sendDTMFRFC2833(c, digit, duration)
	default:
		return u.sendDTMFInfo(c, digit, duration)
	}
}

// sendDTMFRFC2833 通过 RFC 2833 RTP 事件发送 DTMF。
// 构造完整的 DTMF 事件 RTP 包序列（开始 + 冗余 + 结束）并通过传输层发送。
func (u *uac) sendDTMFRFC2833(c *call, digit rune, durationMs int) error {
	// 将数字字符转换为 DTMF 事件
	event, err := media.DigitToEvent(digit)
	if err != nil {
		return fmt.Errorf("uac: invalid DTMF digit %c: %w", digit, err)
	}

	// 创建 DTMF 发送器
	sender := media.NewDTMFSender(0, media.DTMFDefaultClockRate)

	// 使用当前时间戳作为起始点
	timestamp := uint32(time.Now().UnixNano() / int64(time.Millisecond) * 8) // 8000Hz

	// 构建完整的 DTMF 事件 RTP 包序列
	packets := sender.BuildDTMFEventPackets(event, durationMs, timestamp)

	// 通过传输层逐个发送 RTP 包
	for _, pkt := range packets {
		if err := u.tp.SendRaw(pkt.Data, nil, transport.UDP); err != nil {
			u.log.Error("uac", "call %d send DTMF RFC2833 packet: %v", c.handle, err)
			return fmt.Errorf("uac: send DTMF RFC2833: %w", err)
		}
		// 包间间隔约 50ms 以符合 RFC 2833 建议
		time.Sleep(50 * time.Millisecond)
	}

	u.log.Debug("uac", "call %d sent DTMF %c via RFC2833 (%d packets)", c.handle, digit, len(packets))
	return nil
}

// sendDTMFInfo 通过 SIP INFO 方法发送 DTMF。
func (u *uac) sendDTMFInfo(c *call, digit rune, durationMs int) error {
	info := u.buildDTMFInfo(c, digit, durationMs)
	tx, err := u.txMgr.CreateClientTx(info, u.tp)
	if err != nil {
		return fmt.Errorf("uac: create DTMF INFO: %w", err)
	}
	_ = tx

	u.log.Debug("uac", "call %d sent DTMF %c via INFO", c.handle, digit)
	return nil
}

func (u *uac) UpdateMedia(ctx context.Context, handle CallHandle, sdp []byte) error {
	c, err := u.getCall(handle)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.localSDP = sdp
	c.mu.Unlock()

	reinvite := u.buildReInvite(c)
	tx, err := u.txMgr.CreateClientTx(reinvite, u.tp)
	if err != nil {
		return fmt.Errorf("uac: create re-INVITE for media update: %w", err)
	}
	_ = tx
	return nil
}

func (u *uac) GetCall(handle CallHandle) (Call, error) {
	return u.getCall(handle)
}

func (u *uac) SetCallback(cb Callback) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cb = cb
	return nil
}

func (u *uac) GetStats() *Stats {
	return &u.stats
}

func (u *uac) Shutdown(ctx context.Context) error {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return nil
	}
	u.closed = true
	u.mu.Unlock()

	close(u.doneCh)
	u.log.Info("uac", "UAC shutdown")
	return nil
}

func (u *uac) getCall(handle CallHandle) (*call, error) {
	val, ok := u.calls.Load(handle)
	if !ok {
		return nil, fmt.Errorf("uac: call %d not found", handle)
	}
	return val.(*call), nil
}

func (u *uac) setState(c *call, newState CallState) {
	oldState := CallState(c.state.Swap(int32(newState)))
	if oldState != newState && u.cb != nil {
		u.cb.OnCallStateChange(c.handle, newState, oldState)
	}
}

func (u *uac) handleResponse(handle CallHandle, rsp *message.Response) {
	c, err := u.getCall(handle)
	if err != nil {
		return
	}

	if u.cb != nil {
		u.cb.OnIncomingResponse(handle, rsp)
	}

	switch {
	case rsp.IsProvisional():
		if rsp.StatusCode == 180 || rsp.StatusCode == 183 {
			// 更新 remote tag
			if to := rsp.To(); to != nil && to.Tag() != "" {
				c.dialog.RemoteTag = to.Tag()
			}
			if rsp.StatusCode == 183 {
				u.setState(c, CallStateEarly)
			} else {
				u.setState(c, CallStateProceeding)
			}
		}
	case rsp.IsSuccess():
		if to := rsp.To(); to != nil && to.Tag() != "" {
			c.dialog.RemoteTag = to.Tag()
		}
		if len(rsp.Body) > 0 {
			c.mu.Lock()
			c.remoteSDP = rsp.Body
			c.mu.Unlock()
		}
		c.answeredAt = time.Now()
		u.setState(c, CallStateConnected)

		// 记录 setup time
		setupTime := c.answeredAt.Sub(c.createdAt).Nanoseconds()
		u.recordSetupTime(setupTime)

		// 发送 ACK
		ack := u.buildAck(c, rsp)
		u.tp.SendMessage(ack, nil, transport.UDP)

	case rsp.IsFinal():
		u.setState(c, CallStateTerminated)
		u.stats.FailedCalls.Add(1)
		u.stats.ActiveCalls.Add(-1)
	}
}

// recordSetupTime 记录呼叫建立时间并更新 P99。
func (u *uac) recordSetupTime(nanoseconds int64) {
	u.stats.setupTimesMu.Lock()
	u.stats.setupTimes = append(u.stats.setupTimes, nanoseconds)
	// 保留最近 10000 个样本
	if len(u.stats.setupTimes) > 10000 {
		u.stats.setupTimes = u.stats.setupTimes[len(u.stats.setupTimes)-10000:]
	}
	times := make([]int64, len(u.stats.setupTimes))
	copy(times, u.stats.setupTimes)
	u.stats.setupTimesMu.Unlock()

	// 计算平均值
	var total int64
	for _, t := range times {
		total += t
	}
	u.stats.AverageSetupTime.Store(total / int64(len(times)))

	// 计算 P99
	if len(times) >= 100 {
		sorted := make([]int64, len(times))
		copy(sorted, times)
		sortInt64s(sorted)
		p99Idx := int(float64(len(sorted)) * 0.99)
		if p99Idx >= len(sorted) {
			p99Idx = len(sorted) - 1
		}
		u.stats.P99SetupTime.Store(sorted[p99Idx])
	}
}

// sortInt64s 对 int64 切片进行排序。
func sortInt64s(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func (u *uac) buildInvite(callID, localTag string, param *CallParam) *message.Request {
	req := message.NewRequest(message.INVITE, param.ToURI)

	from := &message.NameAddr{
		DisplayName: param.DisplayName,
		Address:     param.FromURI,
		Params:      message.Params{"tag": localTag},
	}
	to := &message.NameAddr{
		Address: param.ToURI,
		Params:  make(message.Params),
	}

	via := &message.Via{
		Transport: "UDP",
		Host:      param.FromURI.Host,
		Params:    message.Params{"branch": message.GenerateBranch()},
	}

	req.Headers.Set(message.HdrFrom, from.String())
	req.Headers.Set(message.HdrTo, to.String())
	req.Headers.Set(message.HdrCallID, callID)
	req.Headers.Set(message.HdrCSeq, (&message.CSeq{SeqNo: 1, Method: message.INVITE}).String())
	req.Headers.Set(message.HdrVia, via.String())
	req.Headers.Set(message.HdrMaxForwards, "70")

	if param.Contact != nil {
		contact := &message.NameAddr{Address: param.Contact}
		req.Headers.Set(message.HdrContact, contact.String())
	}

	if param.UserAgent != "" {
		req.Headers.Set(message.HdrUserAgent, param.UserAgent)
	}

	if len(param.Supported) > 0 {
		req.Headers.Set(message.HdrSupported, joinStrings(param.Supported))
	}

	if len(param.SDP) > 0 {
		req.Body = param.SDP
		req.Headers.Set(message.HdrContentType, "application/sdp")
		req.Headers.Set(message.HdrContentLen, fmt.Sprintf("%d", len(param.SDP)))
	}

	for k, v := range param.ExtraHeaders {
		req.Headers.Set(k, v)
	}

	return req
}

func (u *uac) buildAck(c *call, rsp *message.Response) *message.Request {
	ack := message.NewRequest(message.ACK, c.remoteURI)
	ack.Headers.Set(message.HdrFrom, rsp.Headers.Get(message.HdrFrom))
	ack.Headers.Set(message.HdrTo, rsp.Headers.Get(message.HdrTo))
	ack.Headers.Set(message.HdrCallID, c.dialog.CallID)
	cseq := &message.CSeq{SeqNo: c.dialog.LocalSeq, Method: message.ACK}
	ack.Headers.Set(message.HdrCSeq, cseq.String())
	return ack
}

func (u *uac) buildBye(c *call, reason string) *message.Request {
	bye := message.NewRequest(message.BYE, c.remoteURI)
	bye.Headers.Set(message.HdrFrom, c.dialog.LocalURI.String())
	bye.Headers.Set(message.HdrTo, c.remoteURI.String())
	bye.Headers.Set(message.HdrCallID, c.dialog.CallID)
	c.dialog.LocalSeq++
	cseq := &message.CSeq{SeqNo: c.dialog.LocalSeq, Method: message.BYE}
	bye.Headers.Set(message.HdrCSeq, cseq.String())
	if reason != "" {
		bye.Headers.Set("Reason", fmt.Sprintf("SIP;cause=200;text=%q", reason))
	}
	return bye
}

func (u *uac) buildCancel(c *call) *message.Request {
	cancel := message.NewRequest(message.CANCEL, c.remoteURI)
	cancel.Headers.Set(message.HdrFrom, c.dialog.LocalURI.String())
	cancel.Headers.Set(message.HdrTo, c.remoteURI.String())
	cancel.Headers.Set(message.HdrCallID, c.dialog.CallID)
	cseq := &message.CSeq{SeqNo: c.dialog.LocalSeq, Method: message.CANCEL}
	cancel.Headers.Set(message.HdrCSeq, cseq.String())
	return cancel
}

func (u *uac) buildReInvite(c *call) *message.Request {
	reinvite := message.NewRequest(message.INVITE, c.remoteURI)
	reinvite.Headers.Set(message.HdrFrom, c.dialog.LocalURI.String())
	reinvite.Headers.Set(message.HdrTo, c.remoteURI.String())
	reinvite.Headers.Set(message.HdrCallID, c.dialog.CallID)
	c.dialog.LocalSeq++
	cseq := &message.CSeq{SeqNo: c.dialog.LocalSeq, Method: message.INVITE}
	reinvite.Headers.Set(message.HdrCSeq, cseq.String())

	c.mu.RLock()
	sdp := c.localSDP
	c.mu.RUnlock()
	if len(sdp) > 0 {
		reinvite.Body = sdp
		reinvite.Headers.Set(message.HdrContentType, "application/sdp")
		reinvite.Headers.Set(message.HdrContentLen, fmt.Sprintf("%d", len(sdp)))
	}
	return reinvite
}

func (u *uac) buildRefer(c *call, referTo string, opts *ReferOptions) *message.Request {
	refer := message.NewRequest(message.REFER, c.remoteURI)
	refer.Headers.Set(message.HdrFrom, c.dialog.LocalURI.String())
	refer.Headers.Set(message.HdrTo, c.remoteURI.String())
	refer.Headers.Set(message.HdrCallID, c.dialog.CallID)
	c.dialog.LocalSeq++
	cseq := &message.CSeq{SeqNo: c.dialog.LocalSeq, Method: message.REFER}
	refer.Headers.Set(message.HdrCSeq, cseq.String())
	refer.Headers.Set(message.HdrReferTo, referTo)
	if opts != nil && opts.ReferredBy != "" {
		refer.Headers.Set(message.HdrReferredBy, opts.ReferredBy)
	}
	return refer
}

func (u *uac) buildDTMFInfo(c *call, digit rune, durationMs int) *message.Request {
	info := message.NewRequest(message.INFO, c.remoteURI)
	info.Headers.Set(message.HdrFrom, c.dialog.LocalURI.String())
	info.Headers.Set(message.HdrTo, c.remoteURI.String())
	info.Headers.Set(message.HdrCallID, c.dialog.CallID)
	c.dialog.LocalSeq++
	cseq := &message.CSeq{SeqNo: c.dialog.LocalSeq, Method: message.INFO}
	info.Headers.Set(message.HdrCSeq, cseq.String())
	info.Headers.Set(message.HdrContentType, "application/dtmf-relay")
	body := fmt.Sprintf("Signal=%c\r\nDuration=%d", digit, durationMs)
	info.Body = []byte(body)
	info.Headers.Set(message.HdrContentLen, fmt.Sprintf("%d", len(body)))
	return info
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
