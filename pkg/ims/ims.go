// Package ims 实现 IMS 注册客户端（UE→CSCF）和注册服务器（P-CSCF/S-CSCF）功能。
// 遵循 3GPP TS 24.229 规范。
package ims

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

// RegistrationState 注册状态。
type RegistrationState int32

const (
	RegStateUnregistered RegistrationState = iota
	RegStateRegistering
	RegStateRegistered
	RegStateRefreshing
	RegStateExpired
	RegStateFailed
)

// String 返回注册状态的可读名称。
func (s RegistrationState) String() string {
	switch s {
	case RegStateUnregistered:
		return "Unregistered"
	case RegStateRegistering:
		return "Registering"
	case RegStateRegistered:
		return "Registered"
	case RegStateRefreshing:
		return "Refreshing"
	case RegStateExpired:
		return "Expired"
	case RegStateFailed:
		return "Failed"
	default:
		return "Unknown"
	}
}

// RegisterParam IMS 注册参数。
type RegisterParam struct {
	IMPI           string
	IMPU           *message.URI
	Password       string
	Realm          string
	PCSCF          *message.URI
	Contact        *message.URI
	Expires        int
	InstanceID     string
	RegID          int
	FeatureTags    []string
	UserAgent      string
	SecurityClient string
	SecurityServer string
	SecurityVerify string
}

// Registration IMS 注册实例接口。
type Registration interface {
	ID() string
	State() RegistrationState
	ExpiresAt() time.Time
	IMPU() *message.URI
	IMPI() string
	ServiceRoute() []*message.URI
	PAssociatedURI() []*message.URI
	Update(param *RegisterParam) error
	Destroy() error
}

// Registrar IMS 注册器接口（客户端）。
type Registrar interface {
	Register(ctx context.Context, param *RegisterParam) (Registration, error)
	Refresh(ctx context.Context, reg Registration) error
	Unregister(ctx context.Context, reg Registration) error
	GetStatus(reg Registration) RegistrationState
	GetServiceRoute(reg Registration) ([]*message.URI, error)
	GetPAssociatedURI(reg Registration) ([]*message.URI, error)
	BulkRegister(ctx context.Context, params []*RegisterParam) ([]Registration, error)
}

// RegistrarListener 注册器事件监听器。
type RegistrarListener interface {
	OnRegistrationStateChange(reg Registration, oldState, newState RegistrationState)
	OnRegistrationError(reg Registration, err error)
}

// ContactInfo 注册绑定信息。
type ContactInfo struct {
	URI        *message.URI
	CallID     string
	CSeq       uint32
	Expires    time.Time
	Path       []*message.URI
	InstanceID string
	RegID      int
	QValue     float64
	CreatedAt  time.Time
}

// LocationInfo 位置查询结果。
type LocationInfo struct {
	AOR      string
	Contacts []*ContactInfo
}

// RegisterResponse REGISTER 响应。
type RegisterResponse struct {
	StatusCode     int
	Expires        int
	Contacts       []*message.NameAddr
	ServiceRoute   []*message.URI
	PAssociatedURI []*message.URI
}

// ServerStats IMS 服务器统计。
type ServerStats struct {
	TotalRegistrations  atomic.Int64
	ActiveRegistrations atomic.Int64
	TotalQueries        atomic.Int64
	AverageQueryTime    atomic.Int64
	CacheHitRate        atomic.Int64 // 缓存命中率 * 10000
}

// LocationService 位置服务接口。
type LocationService interface {
	// Lookup 查找 AOR 的绑定。
	Lookup(aor string) ([]*ContactInfo, error)
	// Store 存储绑定。
	Store(aor string, contact *ContactInfo) error
	// Remove 移除绑定。
	Remove(aor string, callID string) error
	// Flush 刷新缓存。
	Flush() error
}

// IMSServer IMS 服务器（CSCF）接口。
type IMSServer interface {
	SetRegistrarListener(listener RegistrarListener) error
	ProcessRegister(ctx context.Context, req *message.Request) (*RegisterResponse, error)
	QueryLocation(ctx context.Context, aor string) (*LocationInfo, error)
	UpdateBinding(ctx context.Context, aor string, contact *ContactInfo) error
	RemoveBinding(ctx context.Context, aor string, callID string) error
	GetBindings(ctx context.Context, aor string) ([]*ContactInfo, error)
	SetLocationService(loc LocationService) error
	GetStats() *ServerStats
	Shutdown(ctx context.Context) error
}

// registration 是 Registration 的默认实现。
type registration struct {
	id             string
	state          atomic.Int32
	expiresAt      time.Time
	impu           *message.URI
	impi           string
	serviceRoute   []*message.URI
	pAssociatedURI []*message.URI
	param          *RegisterParam
	mu             sync.RWMutex
}

func (r *registration) ID() string                     { return r.id }
func (r *registration) State() RegistrationState       { return RegistrationState(r.state.Load()) }
func (r *registration) ExpiresAt() time.Time           { return r.expiresAt }
func (r *registration) IMPU() *message.URI             { return r.impu }
func (r *registration) IMPI() string                   { return r.impi }
func (r *registration) ServiceRoute() []*message.URI   { return r.serviceRoute }
func (r *registration) PAssociatedURI() []*message.URI { return r.pAssociatedURI }
func (r *registration) Update(param *RegisterParam) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.param = param
	return nil
}
func (r *registration) Destroy() error {
	r.state.Store(int32(RegStateUnregistered))
	return nil
}

// registrar 是 Registrar 的默认实现。
type registrar struct {
	txMgr    transaction.Manager
	tp       transport.TransportLayer
	log      logger.Logger
	listener RegistrarListener
	regs     sync.Map
	nextID   atomic.Uint64
}

// NewRegistrar 创建 IMS 注册器。
func NewRegistrar(txMgr transaction.Manager, tp transport.TransportLayer, log logger.Logger) Registrar {
	if log == nil {
		log = logger.NopLogger()
	}
	return &registrar{
		txMgr: txMgr,
		tp:    tp,
		log:   log,
	}
}

func (r *registrar) Register(ctx context.Context, param *RegisterParam) (Registration, error) {
	if param.IMPU == nil || param.PCSCF == nil {
		return nil, errors.New("ims: IMPU and P-CSCF are required")
	}

	id := fmt.Sprintf("reg-%d", r.nextID.Add(1))
	reg := &registration{
		id:    id,
		impu:  param.IMPU,
		impi:  param.IMPI,
		param: param,
	}
	reg.state.Store(int32(RegStateRegistering))

	// 构造 REGISTER 请求
	req := r.buildRegister(param, param.Expires)

	tx, err := r.txMgr.CreateClientTx(req, r.tp)
	if err != nil {
		reg.state.Store(int32(RegStateFailed))
		return nil, fmt.Errorf("ims: create REGISTER transaction: %w", err)
	}

	done := make(chan error, 1)
	tx.SetOnResponse(func(rsp *message.Response) {
		if rsp.IsSuccess() {
			reg.state.Store(int32(RegStateRegistered))
			if param.Expires > 0 {
				reg.expiresAt = time.Now().Add(time.Duration(param.Expires) * time.Second)
			}
			// 解析 Service-Route
			for _, sr := range rsp.Headers.GetAll("Service-Route") {
				if uri, err := message.ParseURI(sr); err == nil {
					reg.serviceRoute = append(reg.serviceRoute, uri)
				}
			}
			// 解析 P-Associated-URI
			for _, pau := range rsp.Headers.GetAll("P-Associated-URI") {
				if uri, err := message.ParseURI(pau); err == nil {
					reg.pAssociatedURI = append(reg.pAssociatedURI, uri)
				}
			}
			done <- nil
		} else if rsp.StatusCode == 401 || rsp.StatusCode == 407 {
			// 认证挑战，需要带凭据重发
			done <- fmt.Errorf("ims: authentication required (%d)", rsp.StatusCode)
		} else {
			reg.state.Store(int32(RegStateFailed))
			done <- fmt.Errorf("ims: registration failed with %d", rsp.StatusCode)
		}
	})

	r.regs.Store(id, reg)
	r.log.Info("ims", "REGISTER sent for %s (id: %s)", param.IMPU, id)

	select {
	case err := <-done:
		if err != nil {
			return reg, err
		}
		return reg, nil
	case <-ctx.Done():
		return reg, ctx.Err()
	}
}

func (r *registrar) Refresh(ctx context.Context, reg Registration) error {
	regImpl, ok := reg.(*registration)
	if !ok {
		return errors.New("ims: invalid registration")
	}
	regImpl.state.Store(int32(RegStateRefreshing))
	_, err := r.Register(ctx, regImpl.param)
	return err
}

func (r *registrar) Unregister(ctx context.Context, reg Registration) error {
	regImpl, ok := reg.(*registration)
	if !ok {
		return errors.New("ims: invalid registration")
	}

	req := r.buildRegister(regImpl.param, 0)
	tx, err := r.txMgr.CreateClientTx(req, r.tp)
	if err != nil {
		return fmt.Errorf("ims: create unREGISTER transaction: %w", err)
	}

	done := make(chan error, 1)
	tx.SetOnResponse(func(rsp *message.Response) {
		if rsp.IsSuccess() {
			regImpl.state.Store(int32(RegStateUnregistered))
			r.regs.Delete(regImpl.id)
			done <- nil
		} else {
			done <- fmt.Errorf("ims: unregister failed with %d", rsp.StatusCode)
		}
	})

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *registrar) GetStatus(reg Registration) RegistrationState {
	return reg.State()
}

func (r *registrar) GetServiceRoute(reg Registration) ([]*message.URI, error) {
	return reg.ServiceRoute(), nil
}

func (r *registrar) GetPAssociatedURI(reg Registration) ([]*message.URI, error) {
	return reg.PAssociatedURI(), nil
}

func (r *registrar) BulkRegister(ctx context.Context, params []*RegisterParam) ([]Registration, error) {
	results := make([]Registration, 0, len(params))
	for _, p := range params {
		reg, err := r.Register(ctx, p)
		if err != nil {
			r.log.Error("ims", "bulk register failed for %s: %v", p.IMPU, err)
			continue
		}
		results = append(results, reg)
	}
	if len(results) == 0 {
		return nil, errors.New("ims: all registrations failed")
	}
	return results, nil
}

func (r *registrar) buildRegister(param *RegisterParam, expires int) *message.Request {
	req := message.NewRequest(message.REGISTER, param.PCSCF)

	from := &message.NameAddr{
		Address: param.IMPU,
		Params:  message.Params{"tag": message.GenerateTag()},
	}
	to := &message.NameAddr{
		Address: param.IMPU,
		Params:  make(message.Params),
	}
	via := &message.Via{
		Transport: "UDP",
		Host:      param.IMPU.Host,
		Params:    message.Params{"branch": message.GenerateBranch()},
	}

	req.Headers.Set(message.HdrFrom, from.String())
	req.Headers.Set(message.HdrTo, to.String())
	req.Headers.Set(message.HdrCallID, message.GenerateCallID())
	req.Headers.Set(message.HdrCSeq, (&message.CSeq{SeqNo: 1, Method: message.REGISTER}).String())
	req.Headers.Set(message.HdrVia, via.String())
	req.Headers.Set(message.HdrMaxForwards, "70")
	req.Headers.Set(message.HdrExpires, fmt.Sprintf("%d", expires))

	if param.Contact != nil {
		contact := &message.NameAddr{Address: param.Contact}
		req.Headers.Set(message.HdrContact, contact.String())
	}

	if param.UserAgent != "" {
		req.Headers.Set(message.HdrUserAgent, param.UserAgent)
	}

	if param.InstanceID != "" {
		req.Headers.Set("Contact", req.Headers.Get(message.HdrContact)+";+sip.instance=\""+param.InstanceID+"\"")
	}

	return req
}

// imsServer 是 IMSServer 的默认实现。
type imsServer struct {
	log      logger.Logger
	listener RegistrarListener
	bindings sync.Map // map[string][]*ContactInfo (AOR -> contacts)
	locSvc   LocationService
	stats    ServerStats
	mu       sync.RWMutex
}

// NewIMSServer 创建 IMS 服务器实例。
func NewIMSServer(log logger.Logger) IMSServer {
	if log == nil {
		log = logger.NopLogger()
	}
	return &imsServer{log: log}
}

func (s *imsServer) SetRegistrarListener(listener RegistrarListener) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listener = listener
	return nil
}

func (s *imsServer) ProcessRegister(ctx context.Context, req *message.Request) (*RegisterResponse, error) {
	expires := 3600
	if v := req.Headers.Get(message.HdrExpires); v != "" {
		fmt.Sscanf(v, "%d", &expires)
	}

	to := req.To()
	aor := ""
	if to != nil && to.Address != nil {
		aor = to.Address.String()
	}

	if expires == 0 {
		// 注销
		s.bindings.Delete(aor)
		s.stats.ActiveRegistrations.Add(-1)
		return &RegisterResponse{StatusCode: 200, Expires: 0}, nil
	}

	// 解析 Contact
	contact := req.Contact()
	if contact != nil {
		ci := &ContactInfo{
			URI:       contact.Address,
			CallID:    req.CallID(),
			Expires:   time.Now().Add(time.Duration(expires) * time.Second),
			CreatedAt: time.Now(),
		}
		s.UpdateBinding(ctx, aor, ci)
	}

	s.stats.TotalRegistrations.Add(1)
	s.log.Info("ims", "REGISTER processed for %s (expires: %d)", aor, expires)

	return &RegisterResponse{
		StatusCode: 200,
		Expires:    expires,
	}, nil
}

func (s *imsServer) QueryLocation(ctx context.Context, aor string) (*LocationInfo, error) {
	start := time.Now()
	defer func() {
		s.stats.TotalQueries.Add(1)
		elapsed := time.Since(start)
		s.stats.AverageQueryTime.Store(elapsed.Nanoseconds())
	}()

	val, ok := s.bindings.Load(aor)
	if !ok {
		return nil, fmt.Errorf("ims: no binding found for %s", aor)
	}

	contacts := val.([]*ContactInfo)
	// 过滤过期绑定
	now := time.Now()
	var active []*ContactInfo
	for _, c := range contacts {
		if c.Expires.After(now) {
			active = append(active, c)
		}
	}

	if len(active) == 0 {
		s.bindings.Delete(aor)
		return nil, fmt.Errorf("ims: all bindings expired for %s", aor)
	}

	return &LocationInfo{AOR: aor, Contacts: active}, nil
}

func (s *imsServer) UpdateBinding(ctx context.Context, aor string, contact *ContactInfo) error {
	val, _ := s.bindings.LoadOrStore(aor, make([]*ContactInfo, 0))
	contacts := val.([]*ContactInfo)

	// 更新或添加
	found := false
	for i, c := range contacts {
		if c.URI.String() == contact.URI.String() {
			contacts[i] = contact
			found = true
			break
		}
	}
	if !found {
		contacts = append(contacts, contact)
		s.stats.ActiveRegistrations.Add(1)
	}

	s.bindings.Store(aor, contacts)
	return nil
}

func (s *imsServer) RemoveBinding(ctx context.Context, aor string, callID string) error {
	val, ok := s.bindings.Load(aor)
	if !ok {
		return nil
	}

	contacts := val.([]*ContactInfo)
	var filtered []*ContactInfo
	for _, c := range contacts {
		if c.CallID != callID {
			filtered = append(filtered, c)
		}
	}

	if len(filtered) == 0 {
		s.bindings.Delete(aor)
	} else {
		s.bindings.Store(aor, filtered)
	}
	return nil
}

func (s *imsServer) GetBindings(ctx context.Context, aor string) ([]*ContactInfo, error) {
	val, ok := s.bindings.Load(aor)
	if !ok {
		return nil, nil
	}
	return val.([]*ContactInfo), nil
}

func (s *imsServer) GetStats() *ServerStats {
	return &s.stats
}

func (s *imsServer) Shutdown(ctx context.Context) error {
	s.log.Info("ims", "IMS server shutdown")
	return nil
}

func (s *imsServer) SetLocationService(loc LocationService) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locSvc = loc
	return nil
}
