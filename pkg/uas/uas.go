// Package uas 实现 UAS（用户代理服务器），负责接收并处理入站呼叫请求。
package uas

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
	"github.com/NuyoahCx330/go-sip-ua/pkg/transaction"
	"github.com/NuyoahCx330/go-sip-ua/pkg/transport"
)

// CallHandle 入站呼叫句柄。
type CallHandle uint64

// IncomingCall 入站呼叫接口。
type IncomingCall interface {
	ID() CallHandle
	CallID() string
	From() *message.NameAddr
	To() *message.NameAddr
	RemoteURI() *message.URI
	LocalURI() *message.URI
	SDP() []byte
	Request() *message.Request
	SetUserData(data interface{})
	UserData() interface{}
	CreatedAt() time.Time
}

// RegisterResult REGISTER 请求处理结果。
type RegisterResult struct {
	StatusCode int
	Reason     string // 自定义原因短语（空则使用标准短语）
	Expires    int
	Contacts   []*message.NameAddr
}

// SubscribeResult SUBSCRIBE 请求处理结果。
type SubscribeResult struct {
	StatusCode int
	Reason     string // 自定义原因短语（空则使用标准短语）
	Expires    int
}

// Listener UAS 事件监听器接口。
type Listener interface {
	OnIncomingCall(handle CallHandle, req *message.Request) bool
	OnIncomingRequest(handle CallHandle, req *message.Request) error
	OnAck(handle CallHandle, ack *message.Request)
	OnCancel(handle CallHandle, cancel *message.Request)
	OnRegister(req *message.Request) (*RegisterResult, error)
	OnSubscribe(req *message.Request) (*SubscribeResult, error)
	OnError(handle CallHandle, err error)
}

// Stats UAS 统计信息。
type Stats struct {
	TotalIncomingCalls atomic.Int64
	ActiveCalls        atomic.Int64
	AnsweredCalls      atomic.Int64
	RejectedCalls      atomic.Int64
	RedirectedCalls    atomic.Int64
}

// UAS 用户代理服务器接口。
type UAS interface {
	SetListener(listener Listener) error
	// HandleRequest 处理入站 SIP 请求（由消息路由层调用）。
	HandleRequest(req *message.Request)
	// AnswerCall 应答呼叫，statusCode 为状态码，reason 为自定义原因短语（空则使用标准短语）。
	AnswerCall(ctx context.Context, handle CallHandle, statusCode int, reason string, sdp []byte) error
	// RejectCall 拒绝呼叫，statusCode 为状态码，reason 为自定义原因短语。
	RejectCall(ctx context.Context, handle CallHandle, statusCode int, reason string) error
	ForwardCall(ctx context.Context, handle CallHandle, target *message.URI) error
	// SendProgress 发送进展响应（1xx），reason 为自定义原因短语。
	SendProgress(ctx context.Context, handle CallHandle, statusCode int, reason string, sdp []byte) error
	// SendResponse 发送自定义响应，完全控制状态码和原因短语。
	SendCustomResponse(ctx context.Context, handle CallHandle, statusCode int, reason string, headers map[string]string, body []byte) error
	RedirectCall(ctx context.Context, handle CallHandle, contacts []*message.NameAddr, statusCode int) error
	GetIncomingCall(handle CallHandle) (IncomingCall, error)
	GetStats() *Stats
	Shutdown(ctx context.Context) error
}

// incomingCall 是 IncomingCall 的实现。
type incomingCall struct {
	handle    CallHandle
	req       *message.Request
	callID    string
	from      *message.NameAddr
	to        *message.NameAddr
	remoteURI *message.URI
	localURI  *message.URI
	sdp       []byte
	userData  interface{}
	createdAt time.Time
	mu        sync.RWMutex
}

func (ic *incomingCall) ID() CallHandle            { return ic.handle }
func (ic *incomingCall) CallID() string            { return ic.callID }
func (ic *incomingCall) From() *message.NameAddr   { return ic.from }
func (ic *incomingCall) To() *message.NameAddr     { return ic.to }
func (ic *incomingCall) RemoteURI() *message.URI   { return ic.remoteURI }
func (ic *incomingCall) LocalURI() *message.URI    { return ic.localURI }
func (ic *incomingCall) SDP() []byte               { return ic.sdp }
func (ic *incomingCall) Request() *message.Request { return ic.req }
func (ic *incomingCall) SetUserData(data interface{}) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	ic.userData = data
}
func (ic *incomingCall) UserData() interface{} {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	return ic.userData
}
func (ic *incomingCall) CreatedAt() time.Time { return ic.createdAt }

// uas UAS 接口的默认实现。
type uas struct {
	txMgr    transaction.Manager
	tp       transport.TransportLayer
	log      logger.Logger
	listener Listener
	calls    sync.Map
	nextID   atomic.Uint64
	stats    Stats
	doneCh   chan struct{}
	mu       sync.RWMutex
	closed   bool
}

// New 创建 UAS 实例。
func New(txMgr transaction.Manager, tp transport.TransportLayer, log logger.Logger) UAS {
	if log == nil {
		log = logger.NopLogger()
	}
	return &uas{
		txMgr:  txMgr,
		tp:     tp,
		log:    log,
		doneCh: make(chan struct{}),
	}
}

func (u *uas) SetListener(listener Listener) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.listener = listener
	return nil
}

func (u *uas) HandleRequest(req *message.Request) {
	switch req.Method {
	case message.INVITE:
		u.handleInvite(req)
	case message.ACK:
		u.handleAck(req)
	case message.BYE:
		u.handleBye(req)
	case message.CANCEL:
		u.handleCancel(req)
	case message.REGISTER:
		u.handleRegister(req)
	case message.OPTIONS:
		u.handleOptions(req)
	case message.SUBSCRIBE:
		u.handleSubscribe(req)
	case message.NOTIFY:
		u.handleNotify(req)
	case message.REFER:
		u.handleRefer(req)
	case message.INFO:
		u.handleInfo(req)
	case message.UPDATE:
		u.handleUpdate(req)
	default:
		u.log.Warn("uas", "unsupported method: %s", req.Method)
		u.sendResponse(req, 405, "Method Not Allowed", nil)
	}
}

func (u *uas) handleInvite(req *message.Request) {
	handle := CallHandle(u.nextID.Add(1))

	ic := &incomingCall{
		handle:    handle,
		req:       req,
		callID:    req.CallID(),
		from:      req.From(),
		to:        req.To(),
		remoteURI: req.RequestURI,
		localURI:  req.RequestURI,
		sdp:       req.Body,
		createdAt: time.Now(),
	}

	u.calls.Store(handle, ic)
	u.stats.TotalIncomingCalls.Add(1)
	u.stats.ActiveCalls.Add(1)

	// 发送 100 Trying
	u.sendResponse(req, 100, "Trying", nil)

	u.mu.RLock()
	listener := u.listener
	u.mu.RUnlock()

	if listener != nil {
		if !listener.OnIncomingCall(handle, req) {
			// 监听器拒绝
			u.RejectCall(context.Background(), handle, 486, "Busy Here")
			return
		}
	}

	u.log.Info("uas", "incoming call %d from %s (Call-ID: %s)", handle, req.From(), req.CallID())
}

func (u *uas) handleAck(req *message.Request) {
	callID := req.CallID()
	u.calls.Range(func(key, value interface{}) bool {
		ic := value.(*incomingCall)
		if ic.callID == callID {
			u.mu.RLock()
			listener := u.listener
			u.mu.RUnlock()
			if listener != nil {
				listener.OnAck(ic.handle, req)
			}
			return false
		}
		return true
	})
}

func (u *uas) handleBye(req *message.Request) {
	callID := req.CallID()
	u.calls.Range(func(key, value interface{}) bool {
		ic := value.(*incomingCall)
		if ic.callID == callID {
			u.sendResponse(req, 200, "OK", nil)
			u.calls.Delete(ic.handle)
			u.stats.ActiveCalls.Add(-1)
			return false
		}
		return true
	})
}

func (u *uas) handleCancel(req *message.Request) {
	callID := req.CallID()
	u.calls.Range(func(key, value interface{}) bool {
		ic := value.(*incomingCall)
		if ic.callID == callID {
			u.sendResponse(ic.req, 487, "Request Terminated", nil)
			u.sendResponse(req, 200, "OK", nil)
			u.calls.Delete(ic.handle)
			u.stats.ActiveCalls.Add(-1)

			u.mu.RLock()
			listener := u.listener
			u.mu.RUnlock()
			if listener != nil {
				listener.OnCancel(ic.handle, req)
			}
			return false
		}
		return true
	})
}

func (u *uas) handleRegister(req *message.Request) {
	u.mu.RLock()
	listener := u.listener
	u.mu.RUnlock()

	if listener != nil {
		result, err := listener.OnRegister(req)
		if err != nil {
			u.sendResponse(req, 500, "Server Internal Error", nil)
			return
		}
		if result != nil {
			// 使用 RegisterResult 中的 StatusCode，reason 保留标准短语
			// 但调用方可通过扩展 RegisterResult 自定义
			reason := result.Reason
			if reason == "" {
				reason = message.ReasonPhrase(result.StatusCode)
			}
			rsp := message.NewResponse(result.StatusCode, reason)
			copyDialogHeaders(req, rsp)
			if result.Expires > 0 {
				rsp.Headers.Set(message.HdrExpires, fmt.Sprintf("%d", result.Expires))
			}
			u.tp.SendMessage(rsp, nil, transport.UDP)
			return
		}
	}
	u.sendResponse(req, 200, "OK", nil)
}

func (u *uas) handleOptions(req *message.Request) {
	rsp := message.NewResponse(200, "OK")
	copyDialogHeaders(req, rsp)
	rsp.Headers.Set(message.HdrAllow, "INVITE, ACK, BYE, CANCEL, OPTIONS, REGISTER, SUBSCRIBE, NOTIFY, REFER, INFO, UPDATE")
	u.tp.SendMessage(rsp, nil, transport.UDP)
}

func (u *uas) handleSubscribe(req *message.Request) {
	u.mu.RLock()
	listener := u.listener
	u.mu.RUnlock()

	if listener != nil {
		result, err := listener.OnSubscribe(req)
		if err != nil {
			u.sendResponse(req, 500, "Server Internal Error", nil)
			return
		}
		if result != nil {
			reason := result.Reason
			if reason == "" {
				reason = message.ReasonPhrase(result.StatusCode)
			}
			rsp := message.NewResponse(result.StatusCode, reason)
			copyDialogHeaders(req, rsp)
			if result.Expires > 0 {
				rsp.Headers.Set(message.HdrExpires, fmt.Sprintf("%d", result.Expires))
			}
			u.tp.SendMessage(rsp, nil, transport.UDP)
			return
		}
	}
	u.sendResponse(req, 200, "OK", nil)
}

func (u *uas) handleNotify(req *message.Request) {
	u.sendResponse(req, 200, "OK", nil)
}

func (u *uas) handleRefer(req *message.Request) {
	u.sendResponse(req, 202, "Accepted", nil)
}

func (u *uas) handleInfo(req *message.Request) {
	u.sendResponse(req, 200, "OK", nil)
}

func (u *uas) handleUpdate(req *message.Request) {
	u.sendResponse(req, 200, "OK", nil)
}

func (u *uas) AnswerCall(ctx context.Context, handle CallHandle, statusCode int, reason string, sdp []byte) error {
	ic, err := u.getIncomingCall(handle)
	if err != nil {
		return err
	}

	// 如果未指定 reason，使用标准短语；否则使用调用方自定义的短语
	if reason == "" {
		reason = message.ReasonPhrase(statusCode)
	}
	rsp := message.NewResponse(statusCode, reason)
	copyDialogHeaders(ic.req, rsp)

	if to := rsp.To(); to != nil {
		if to.Tag() == "" {
			to.SetTag(message.GenerateTag())
		}
	}

	if len(sdp) > 0 {
		rsp.Body = sdp
		rsp.Headers.Set(message.HdrContentType, "application/sdp")
		rsp.Headers.Set(message.HdrContentLen, fmt.Sprintf("%d", len(sdp)))
	}

	u.tp.SendMessage(rsp, nil, transport.UDP)
	u.stats.AnsweredCalls.Add(1)
	u.log.Info("uas", "call %d answered with %d", handle, statusCode)
	return nil
}

func (u *uas) RejectCall(ctx context.Context, handle CallHandle, statusCode int, reason string) error {
	ic, err := u.getIncomingCall(handle)
	if err != nil {
		return err
	}

	if reason == "" {
		reason = message.ReasonPhrase(statusCode)
	}
	u.sendResponse(ic.req, statusCode, reason, nil)
	u.calls.Delete(handle)
	u.stats.ActiveCalls.Add(-1)
	u.stats.RejectedCalls.Add(1)
	u.log.Info("uas", "call %d rejected: %d %s", handle, statusCode, reason)
	return nil
}

func (u *uas) ForwardCall(ctx context.Context, handle CallHandle, target *message.URI) error {
	ic, err := u.getIncomingCall(handle)
	if err != nil {
		return err
	}

	// 302 Moved Temporarily
	rsp := message.NewResponse(302, "Moved Temporarily")
	copyDialogHeaders(ic.req, rsp)
	contact := &message.NameAddr{Address: target}
	rsp.Headers.Set(message.HdrContact, contact.String())
	u.tp.SendMessage(rsp, nil, transport.UDP)
	u.stats.RedirectedCalls.Add(1)
	return nil
}

func (u *uas) SendProgress(ctx context.Context, handle CallHandle, statusCode int, reason string, sdp []byte) error {
	ic, err := u.getIncomingCall(handle)
	if err != nil {
		return err
	}

	// 如果未指定 reason，使用标准短语；否则使用调用方自定义的短语
	if reason == "" {
		reason = message.ReasonPhrase(statusCode)
	}
	u.sendResponse(ic.req, statusCode, reason, sdp)
	return nil
}

func (u *uas) SendCustomResponse(ctx context.Context, handle CallHandle, statusCode int, reason string, headers map[string]string, body []byte) error {
	ic, err := u.getIncomingCall(handle)
	if err != nil {
		return err
	}

	if reason == "" {
		reason = message.ReasonPhrase(statusCode)
	}
	rsp := message.NewResponse(statusCode, reason)
	copyDialogHeaders(ic.req, rsp)

	// 设置自定义头域
	for k, v := range headers {
		rsp.Headers.Set(k, v)
	}

	if len(body) > 0 {
		rsp.Body = body
		rsp.Headers.Set(message.HdrContentType, "application/sdp")
		rsp.Headers.Set(message.HdrContentLen, fmt.Sprintf("%d", len(body)))
	}

	u.tp.SendMessage(rsp, nil, transport.UDP)
	u.log.Info("uas", "call %d custom response: %d %s", handle, statusCode, reason)
	return nil
}

func (u *uas) RedirectCall(ctx context.Context, handle CallHandle, contacts []*message.NameAddr, statusCode int) error {
	ic, err := u.getIncomingCall(handle)
	if err != nil {
		return err
	}

	// 使用标准短语，但调用方可通过 SendCustomResponse 自定义
	rsp := message.NewResponse(statusCode, message.ReasonPhrase(statusCode))
	copyDialogHeaders(ic.req, rsp)
	for _, c := range contacts {
		rsp.Headers.Add(message.HdrContact, c.String())
	}
	u.tp.SendMessage(rsp, nil, transport.UDP)
	u.stats.RedirectedCalls.Add(1)
	return nil
}

func (u *uas) GetIncomingCall(handle CallHandle) (IncomingCall, error) {
	return u.getIncomingCall(handle)
}

func (u *uas) GetStats() *Stats {
	return &u.stats
}

func (u *uas) Shutdown(ctx context.Context) error {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return nil
	}
	u.closed = true
	u.mu.Unlock()
	close(u.doneCh)
	u.log.Info("uas", "UAS shutdown")
	return nil
}

func (u *uas) getIncomingCall(handle CallHandle) (*incomingCall, error) {
	val, ok := u.calls.Load(handle)
	if !ok {
		return nil, fmt.Errorf("uas: call %d not found", handle)
	}
	return val.(*incomingCall), nil
}

func (u *uas) sendResponse(req *message.Request, statusCode int, reason string, sdp []byte) {
	rsp := message.NewResponse(statusCode, reason)
	copyDialogHeaders(req, rsp)
	if len(sdp) > 0 {
		rsp.Body = sdp
		rsp.Headers.Set(message.HdrContentType, "application/sdp")
		rsp.Headers.Set(message.HdrContentLen, fmt.Sprintf("%d", len(sdp)))
	}
	if err := u.tp.SendMessage(rsp, nil, transport.UDP); err != nil {
		u.log.Error("uas", "send response %d: %v", statusCode, err)
	}
}

// copyDialogHeaders 从请求中复制对话相关头域到响应。
func copyDialogHeaders(req *message.Request, rsp *message.Response) {
	rsp.Headers.Set(message.HdrFrom, req.Headers.Get(message.HdrFrom))
	rsp.Headers.Set(message.HdrTo, req.Headers.Get(message.HdrTo))
	rsp.Headers.Set(message.HdrCallID, req.Headers.Get(message.HdrCallID))
	rsp.Headers.Set(message.HdrCSeq, req.Headers.Get(message.HdrCSeq))

	// 复制 Via（倒序）
	for _, v := range req.Headers.GetAll(message.HdrVia) {
		rsp.Headers.Add(message.HdrVia, v)
	}
}

// Ensure unused imports are referenced
var _ = errors.New
