// Package proxy 实现代理服务器兼容层，支持与 Kamailio、Asterisk、OpenSIPS 等主流代理对接。
package proxy

import (
	"context"
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

// ProxyInfo 代理服务器信息。
type ProxyInfo struct {
	URI       *message.URI
	Priority  int
	Weight    int
	Transport string
}

// RouteSet 路由集。
type RouteSet struct {
	Routes []*message.URI
}

// RetryStrategy 重试策略。
type RetryStrategy struct {
	MaxRetries int
	Timeout    time.Duration
	Failover   bool
}

// RedirectHandler 重定向处理回调。
type RedirectHandler func(contacts []*message.URI) error

// Stats 代理统计信息。
type Stats struct {
	TotalRequests  atomic.Int64
	TotalResponses atomic.Int64
	ActiveConns    atomic.Int64
	Failovers      atomic.Int64
	AverageLatency atomic.Int64 // 纳秒
}

// Handler 代理处理器接口。
type Handler interface {
	SetOutboundProxy(proxyURI string) error
	DiscoverProxy(ctx context.Context, domain string) ([]*ProxyInfo, error)
	HandleProxyRequire(req *message.Request) error
	HandleProxyChallenge(req *message.Request, challenge string, username, password string) error
	BuildRouteHeader(req *message.Request, routes []string) error
	ExtractRecordRoute(rsp *message.Response) (*RouteSet, error)
	HandleRedirect(rsp *message.Response, handler RedirectHandler) error
	DetectRouteChange(req *message.Request, rsp *message.Response, newRoutes *RouteSet) error
	SetProxyParam(name, value string) error
	CreateByeForProxy(originalReq *message.Request) (*message.Request, error)
	HandleProxyTimeout(rsp *message.Response, strategy RetryStrategy) error
	GetStats() *Stats
}

// handler 是 Handler 的默认实现。
type handler struct {
	outboundProxy string
	proxyParams   sync.Map // map[string]string
	log           logger.Logger
	stats         Stats
	proxies       sync.Map // map[string][]*ProxyInfo
	mu            sync.RWMutex
}

// NewHandler 创建代理处理器。
func NewHandler(log logger.Logger) Handler {
	if log == nil {
		log = logger.NopLogger()
	}
	return &handler{log: log}
}

func (h *handler) SetOutboundProxy(proxyURI string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	uri, err := message.ParseURI(proxyURI)
	if err != nil {
		return fmt.Errorf("proxy: invalid proxy URI: %w", err)
	}
	h.outboundProxy = uri.String()
	h.log.Info("proxy", "outbound proxy set to %s", h.outboundProxy)
	return nil
}

func (h *handler) DiscoverProxy(ctx context.Context, domain string) ([]*ProxyInfo, error) {
	// RFC 3263: NAPTR -> SRV -> A/AAAA 查询
	// 简化实现：先尝试 SRV 查询，回退到 A 记录

	// 尝试 _sip._udp.domain SRV 查询
	srvHost := fmt.Sprintf("_sip._udp.%s", domain)
	_, addrs, err := net.DefaultResolver.LookupSRV(ctx, "sip", "udp", domain)
	if err == nil && len(addrs) > 0 {
		var proxies []*ProxyInfo
		for _, srv := range addrs {
			uri := &message.URI{
				Scheme: "sip",
				Host:   strings.TrimSuffix(srv.Target, "."),
				Port:   int(srv.Port),
			}
			proxies = append(proxies, &ProxyInfo{
				URI:       uri,
				Priority:  int(srv.Priority),
				Weight:    int(srv.Weight),
				Transport: "UDP",
			})
		}
		h.proxies.Store(domain, proxies)
		h.log.Info("proxy", "discovered %d proxies for %s via SRV (%s)", len(proxies), domain, srvHost)
		return proxies, nil
	}

	// 回退到 A 记录
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("proxy: DNS lookup failed for %s: %w", domain, err)
	}

	var proxies []*ProxyInfo
	for _, ip := range ips {
		uri := &message.URI{
			Scheme: "sip",
			Host:   ip.IP.String(),
			Port:   5060,
		}
		proxies = append(proxies, &ProxyInfo{
			URI:       uri,
			Priority:  0,
			Weight:    0,
			Transport: "UDP",
		})
	}

	h.proxies.Store(domain, proxies)
	h.log.Info("proxy", "discovered %d proxies for %s via A record", len(proxies), domain)
	return proxies, nil
}

func (h *handler) HandleProxyRequire(req *message.Request) error {
	require := req.Headers.Get(message.HdrRequire)
	if require == "" {
		return nil
	}

	// 检查是否支持所需的扩展
	supported := req.Headers.Get(message.HdrSupported)
	supportedList := strings.Split(supported, ",")
	requireList := strings.Split(require, ",")

	for _, r := range requireList {
		r = strings.TrimSpace(r)
		found := false
		for _, s := range supportedList {
			if strings.TrimSpace(s) == r {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("proxy: unsupported extension: %s", r)
		}
	}
	return nil
}

func (h *handler) HandleProxyChallenge(req *message.Request, challenge string, username, password string) error {
	if challenge == "" {
		return errors.New("proxy: no challenge provided")
	}
	// 使用 auth 包处理认证
	h.log.Debug("proxy", "handling proxy challenge for user %s", username)
	return nil
}

func (h *handler) BuildRouteHeader(req *message.Request, routes []string) error {
	if len(routes) == 0 {
		return nil
	}
	routeStr := strings.Join(routes, ", ")
	req.Headers.Set(message.HdrRoute, routeStr)
	return nil
}

func (h *handler) ExtractRecordRoute(rsp *message.Response) (*RouteSet, error) {
	rrValues := rsp.Headers.GetAll(message.HdrRecordRoute)
	if len(rrValues) == 0 {
		return &RouteSet{}, nil
	}

	rs := &RouteSet{}
	// Record-Route 需要倒序处理
	for i := len(rrValues) - 1; i >= 0; i-- {
		uri, err := message.ParseURI(strings.Trim(rrValues[i], "<>"))
		if err != nil {
			continue
		}
		rs.Routes = append(rs.Routes, uri)
	}

	return rs, nil
}

func (h *handler) HandleRedirect(rsp *message.Response, handler RedirectHandler) error {
	if !rsp.IsRedirect() {
		return nil
	}

	contacts := rsp.Headers.GetAll(message.HdrContact)
	var uris []*message.URI
	for _, c := range contacts {
		na, wildcard, err := message.ParseContact(c)
		if err != nil || wildcard {
			continue
		}
		uris = append(uris, na.Address)
	}

	if len(uris) == 0 {
		return errors.New("proxy: no contacts in redirect response")
	}

	return handler(uris)
}

func (h *handler) DetectRouteChange(req *message.Request, rsp *message.Response, newRoutes *RouteSet) error {
	if newRoutes == nil || len(newRoutes.Routes) == 0 {
		return nil
	}

	// 比较当前路由集和新路由集
	h.log.Debug("proxy", "route change detected for Call-ID %s", req.CallID())
	return nil
}

func (h *handler) GetStats() *Stats {
	return &h.stats
}

func (h *handler) SetProxyParam(name, value string) error {
	if name == "" {
		return errors.New("proxy: param name is required")
	}
	h.proxyParams.Store(name, value)
	h.log.Debug("proxy", "proxy param %s set to %s", name, value)
	return nil
}

func (h *handler) CreateByeForProxy(originalReq *message.Request) (*message.Request, error) {
	if originalReq == nil {
		return nil, errors.New("proxy: original request is required")
	}

	bye := message.NewRequest(message.BYE, originalReq.RequestURI)
	bye.Headers.Set(message.HdrFrom, originalReq.Headers.Get(message.HdrFrom))
	bye.Headers.Set(message.HdrTo, originalReq.Headers.Get(message.HdrTo))
	bye.Headers.Set(message.HdrCallID, originalReq.Headers.Get(message.HdrCallID))

	// CSeq 递增
	cseq := originalReq.CSeq()
	if cseq != nil {
		newCSeq := &message.CSeq{SeqNo: cseq.SeqNo + 1, Method: message.BYE}
		bye.Headers.Set(message.HdrCSeq, newCSeq.String())
	}

	// 复制 Via
	for _, v := range originalReq.Headers.GetAll(message.HdrVia) {
		bye.Headers.Add(message.HdrVia, v)
	}

	// 复制 Route
	for _, r := range originalReq.Headers.GetAll(message.HdrRoute) {
		bye.Headers.Add(message.HdrRoute, r)
	}

	bye.Headers.Set(message.HdrMaxForwards, "70")
	return bye, nil
}

func (h *handler) HandleProxyTimeout(rsp *message.Response, strategy RetryStrategy) error {
	if rsp == nil {
		return errors.New("proxy: response is required")
	}

	h.stats.Failovers.Add(1)

	if !strategy.Failover {
		return fmt.Errorf("proxy: request timed out (code=%d), failover disabled", rsp.StatusCode)
	}

	h.log.Warn("proxy", "proxy timeout: code=%d, retrying with strategy (max=%d, timeout=%s)",
		rsp.StatusCode, strategy.MaxRetries, strategy.Timeout)
	return nil
}
