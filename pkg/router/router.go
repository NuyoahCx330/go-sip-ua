// Package router 提供 SIP 请求路由功能。
// 支持基于号码、正则、头域的路由规则，以及路由组和负载均衡。
package router

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
)

// RouteAction 路由动作类型。
type RouteAction string

const (
	RouteActionProxy    RouteAction = "proxy"    // 代理转发
	RouteActionReject   RouteAction = "reject"   // 拒绝
	RouteActionRedirect RouteAction = "redirect" // 重定向
	RouteActionDrop     RouteAction = "drop"     // 丢弃
)

// RouteEntry 路由表条目。
type RouteEntry struct {
	// ID 路由条目唯一标识。
	ID string
	// Priority 优先级（数字越小优先级越高）。
	Priority int
	// MatchType 匹配类型。
	MatchType MatchType
	// MatchPattern 匹配模式（正则表达式或前缀）。
	MatchPattern string
	// compiledPattern 编译后的正则。
	compiledPattern *regexp.Regexp
	// Action 路由动作。
	Action RouteAction
	// TargetURI 目标 URI（proxy 动作时使用）。
	TargetURI string
	// TargetGroup 目标路由组（负载均衡时使用）。
	TargetGroup string
	// RejectCode 拒绝状态码。
	RejectCode int
	// RejectReason 拒绝原因短语。
	RejectReason string
	// RewriteFrom 重写主叫号码。
	RewriteFrom string
	// RewriteTo 重写被叫号码。
	RewriteTo string
	// RewritePrefix 被叫号码前缀。
	RewritePrefix string
	// StripPrefix 剥离前缀。
	StripPrefix string
	// Headers 附加头域。
	Headers map[string]string
	// Enabled 是否启用。
	Enabled bool
	// Description 描述。
	Description string
}

// MatchType 匹配类型。
type MatchType string

const (
	MatchExact    MatchType = "exact"    // 精确匹配
	MatchPrefix   MatchType = "prefix"   // 前缀匹配
	MatchRegex    MatchType = "regex"    // 正则匹配
	MatchWildcard MatchType = "wildcard" // 通配符匹配
)

// RouteGroup 路由组（用于负载均衡和故障转移）。
type RouteGroup struct {
	ID       string
	Members  []*RouteGroupMember
	Strategy LoadBalanceStrategy
	mu       sync.RWMutex
}

// RouteGroupMember 路由组成员。
type RouteGroupMember struct {
	URI      string
	Priority int
	Weight   int
	Active   bool
	Failures atomic.Int64
}

// LoadBalanceStrategy 负载均衡策略。
type LoadBalanceStrategy string

const (
	LBRoundRobin LoadBalanceStrategy = "round_robin"
	LBWeighted   LoadBalanceStrategy = "weighted"
	LBPriority   LoadBalanceStrategy = "priority"
	LBLeastConn  LoadBalanceStrategy = "least_conn"
)

// RouteResult 路由结果。
type RouteResult struct {
	// Action 路由动作。
	Action RouteAction
	// TargetURI 目标 URI。
	TargetURI string
	// RejectCode 拒绝状态码。
	RejectCode int
	// RejectReason 拒绝原因短语。
	RejectReason string
	// MatchedEntry 匹配的路由条目。
	MatchedEntry *RouteEntry
	// RewriteFrom 重写后的主叫。
	RewriteFrom string
	// RewriteTo 重写后的被叫。
	RewriteTo string
	// Headers 附加头域。
	Headers map[string]string
}

// Router SIP 路由器接口。
type Router interface {
	// Route 路由 SIP 请求。
	Route(ctx context.Context, req *message.Request) (*RouteResult, error)
	// AddEntry 添加路由条目。
	AddEntry(entry *RouteEntry) error
	// RemoveEntry 移除路由条目。
	RemoveEntry(id string) error
	// UpdateEntry 更新路由条目。
	UpdateEntry(entry *RouteEntry) error
	// GetEntries 获取所有路由条目。
	GetEntries() []*RouteEntry
	// AddGroup 添加路由组。
	AddGroup(group *RouteGroup) error
	// RemoveGroup 移除路由组。
	RemoveGroup(id string) error
	// GetGroup 获取路由组。
	GetGroup(id string) *RouteGroup
	// SelectGroupMember 从路由组中选择成员。
	SelectGroupMember(groupID string) (*RouteGroupMember, error)
	// GetStats 获取路由统计。
	GetStats() *RouterStats
}

// RouterStats 路由统计。
type RouterStats struct {
	TotalRoutes      atomic.Int64
	TotalMatched     atomic.Int64
	TotalRejected    atomic.Int64
	TotalProxied     atomic.Int64
	TotalFailed      atomic.Int64
	AverageRouteTime atomic.Int64 // 纳秒
}

// router 路由器默认实现。
type router struct {
	entries []*RouteEntry
	groups  sync.Map // map[string]*RouteGroup
	log     logger.Logger
	stats   RouterStats
	rrIndex atomic.Uint64 // Round-robin 计数器
	mu      sync.RWMutex
}

// NewRouter 创建 SIP 路由器。
func NewRouter(log logger.Logger) Router {
	if log == nil {
		log = logger.NopLogger()
	}
	return &router{log: log}
}

func (r *router) Route(ctx context.Context, req *message.Request) (*RouteResult, error) {
	start := time.Now()
	defer func() {
		elapsed := time.Since(start)
		r.stats.TotalRoutes.Add(1)
		// 指数加权移动平均
		old := r.stats.AverageRouteTime.Load()
		if old == 0 {
			r.stats.AverageRouteTime.Store(elapsed.Nanoseconds())
		} else {
			r.stats.AverageRouteTime.Store((old*7 + elapsed.Nanoseconds()) / 8)
		}
	}()

	// 提取被叫号码
	toUser := ""
	if req.RequestURI != nil {
		toUser = req.RequestURI.User
	}
	if toUser == "" {
		to := req.To()
		if to != nil && to.Address != nil {
			toUser = to.Address.User
		}
	}

	// 按优先级排序路由条目
	r.mu.RLock()
	entries := make([]*RouteEntry, len(r.entries))
	copy(entries, r.entries)
	r.mu.RUnlock()

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Priority < entries[j].Priority
	})

	// 匹配路由
	for _, entry := range entries {
		if !entry.Enabled {
			continue
		}
		if !r.matchEntry(entry, toUser, req) {
			continue
		}

		r.stats.TotalMatched.Add(1)

		result := &RouteResult{
			Action:       entry.Action,
			MatchedEntry: entry,
			Headers:      entry.Headers,
		}

		switch entry.Action {
		case RouteActionProxy:
			targetURI := entry.TargetURI
			// 路由组负载均衡
			if entry.TargetGroup != "" {
				member, err := r.SelectGroupMember(entry.TargetGroup)
				if err != nil {
					r.stats.TotalFailed.Add(1)
					return nil, fmt.Errorf("router: group %s: %w", entry.TargetGroup, err)
				}
				targetURI = member.URI
			}
			// 号码重写
			rewrittenTo := r.rewriteNumber(toUser, entry)
			result.TargetURI = targetURI
			result.RewriteTo = rewrittenTo
			r.stats.TotalProxied.Add(1)

		case RouteActionReject:
			result.RejectCode = entry.RejectCode
			result.RejectReason = entry.RejectReason
			if result.RejectCode == 0 {
				result.RejectCode = 403
				result.RejectReason = "Forbidden"
			}
			r.stats.TotalRejected.Add(1)

		case RouteActionRedirect:
			result.TargetURI = entry.TargetURI

		case RouteActionDrop:
			// 静默丢弃
		}

		r.log.Debug("router", "matched route %s for %s -> %s %s",
			entry.ID, toUser, entry.Action, result.TargetURI)
		return result, nil
	}

	// 无匹配路由
	return nil, fmt.Errorf("router: no matching route for %s", toUser)
}

func (r *router) matchEntry(entry *RouteEntry, toUser string, req *message.Request) bool {
	switch entry.MatchType {
	case MatchExact:
		return toUser == entry.MatchPattern

	case MatchPrefix:
		return strings.HasPrefix(toUser, entry.MatchPattern)

	case MatchRegex:
		if entry.compiledPattern == nil {
			return false
		}
		return entry.compiledPattern.MatchString(toUser)

	case MatchWildcard:
		return matchWildcard(toUser, entry.MatchPattern)

	default:
		return false
	}
}

func matchWildcard(s, pattern string) bool {
	// 简单通配符匹配：* 匹配任意字符
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return s == pattern
	}
	// 前缀匹配
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	// 中间部分匹配
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	// 后缀匹配
	return strings.HasSuffix(s, parts[len(parts)-1])
}

func (r *router) rewriteNumber(toUser string, entry *RouteEntry) string {
	result := toUser

	// 剥离前缀
	if entry.StripPrefix != "" {
		result = strings.TrimPrefix(result, entry.StripPrefix)
	}

	// 添加前缀
	if entry.RewritePrefix != "" {
		result = entry.RewritePrefix + result
	}

	// 完全重写
	if entry.RewriteTo != "" {
		result = entry.RewriteTo
	}

	return result
}

func (r *router) AddEntry(entry *RouteEntry) error {
	if entry.ID == "" {
		return errors.New("router: entry ID is required")
	}

	// 编译正则
	if entry.MatchType == MatchRegex && entry.MatchPattern != "" {
		compiled, err := regexp.Compile(entry.MatchPattern)
		if err != nil {
			return fmt.Errorf("router: invalid regex pattern: %w", err)
		}
		entry.compiledPattern = compiled
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
	return nil
}

func (r *router) RemoveEntry(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.entries {
		if e.ID == id {
			r.entries = append(r.entries[:i], r.entries[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("router: entry %s not found", id)
}

func (r *router) UpdateEntry(entry *RouteEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.entries {
		if e.ID == entry.ID {
			// 重新编译正则
			if entry.MatchType == MatchRegex && entry.MatchPattern != "" {
				compiled, err := regexp.Compile(entry.MatchPattern)
				if err != nil {
					return fmt.Errorf("router: invalid regex pattern: %w", err)
				}
				entry.compiledPattern = compiled
			}
			r.entries[i] = entry
			return nil
		}
	}
	return fmt.Errorf("router: entry %s not found", entry.ID)
}

func (r *router) GetEntries() []*RouteEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*RouteEntry, len(r.entries))
	copy(result, r.entries)
	return result
}

func (r *router) AddGroup(group *RouteGroup) error {
	if group.ID == "" {
		return errors.New("router: group ID is required")
	}
	r.groups.Store(group.ID, group)
	return nil
}

func (r *router) RemoveGroup(id string) error {
	r.groups.Delete(id)
	return nil
}

func (r *router) GetGroup(id string) *RouteGroup {
	val, ok := r.groups.Load(id)
	if !ok {
		return nil
	}
	return val.(*RouteGroup)
}

func (r *router) SelectGroupMember(groupID string) (*RouteGroupMember, error) {
	val, ok := r.groups.Load(groupID)
	if !ok {
		return nil, fmt.Errorf("router: group %s not found", groupID)
	}
	group := val.(*RouteGroup)

	group.mu.RLock()
	defer group.mu.RUnlock()

	if len(group.Members) == 0 {
		return nil, fmt.Errorf("router: group %s has no members", groupID)
	}

	// 过滤活跃成员
	var active []*RouteGroupMember
	for _, m := range group.Members {
		if m.Active {
			active = append(active, m)
		}
	}
	if len(active) == 0 {
		return nil, fmt.Errorf("router: group %s has no active members", groupID)
	}

	switch group.Strategy {
	case LBRoundRobin:
		idx := r.rrIndex.Add(1) % uint64(len(active))
		return active[idx], nil

	case LBWeighted:
		totalWeight := 0
		for _, m := range active {
			totalWeight += m.Weight
		}
		if totalWeight == 0 {
			return active[0], nil
		}
		target := int(r.rrIndex.Add(1)) % totalWeight
		current := 0
		for _, m := range active {
			current += m.Weight
			if target < current {
				return m, nil
			}
		}
		return active[len(active)-1], nil

	case LBPriority:
		// 按优先级排序，选择最高优先级的活跃成员
		best := active[0]
		for _, m := range active[1:] {
			if m.Priority < best.Priority {
				best = m
			}
		}
		return best, nil

	default:
		return active[0], nil
	}
}

func (r *router) GetStats() *RouterStats {
	return &r.stats
}
