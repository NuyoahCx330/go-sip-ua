// Package headermanip 提供 SIP 头域操作功能，支持基于模板的动态头域生成。
package headermanip

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
)

// Action 头域操作类型。
type Action string

const (
	// ActionAdd 添加头域。
	ActionAdd Action = "add"
	// ActionSet 设置头域（覆盖）。
	ActionSet Action = "set"
	// ActionRemove 删除头域。
	ActionRemove Action = "remove"
	// ActionCopy 从其他头域复制。
	ActionCopy Action = "copy"
)

// ResponseRewriteRule 响应改写规则。
// 用于在转发响应给主叫时改写状态码和原因短语。
type ResponseRewriteRule struct {
	// MatchCode 匹配的状态码（0 表示匹配所有）。
	MatchCode int
	// MatchReason 匹配的原因短语正则（空表示匹配所有）。
	MatchReason string
	// NewCode 改写后的状态码（0 表示不改变）。
	NewCode int
	// NewReason 改写后的原因短语（空表示不改变）。
	NewReason string
	// Condition 条件表达式（可选）。
	Condition string
	// compiledReason 编译后的正则。
	compiledReason *regexp.Regexp
}

// ResponseRewriter 响应改写器接口。
type ResponseRewriter interface {
	// RewriteResponse 根据规则改写响应。
	// 返回改写后的状态码、原因短语和是否被修改。
	RewriteResponse(code int, reason string, rules []*ResponseRewriteRule) (int, string, bool)
	// AddRule 添加改写规则。
	AddRule(rule *ResponseRewriteRule) error
	// Rules 返回所有规则。
	Rules() []*ResponseRewriteRule
}

// Rule 头域处理规则。
type Rule struct {
	// Action 操作类型。
	Action Action
	// Name 头域名称。
	Name string
	// Value 头域值（支持模板变量）。
	Value string
	// SourceHeader 源头域名称（用于 copy 操作）。
	SourceHeader string
	// Condition 条件表达式（可选）。
	Condition string
}

// Manipulator 头域操作器接口。
type Manipulator interface {
	// ApplyRules 应用头域处理规则到请求。
	ApplyRules(req *message.Request, rules []*Rule) error
	// ApplyRulesToResponse 应用头域处理规则到响应。
	ApplyRulesToResponse(rsp *message.Response, rules []*Rule) error
	// EvaluateTemplate 评估模板表达式。
	EvaluateTemplate(template string, req *message.Request) (string, error)
	// CopyHeader 复制头域值。
	CopyHeader(headers *message.Headers, from, to string) error
}

// manipulator 是 Manipulator 的默认实现。
type manipulator struct{}

// responseRewriter 是 ResponseRewriter 的默认实现。
type responseRewriter struct {
	rules []*ResponseRewriteRule
	mu    sync.RWMutex
}

// NewResponseRewriter 创建响应改写器。
func NewResponseRewriter() ResponseRewriter {
	return &responseRewriter{}
}

func (rw *responseRewriter) AddRule(rule *ResponseRewriteRule) error {
	if rule.MatchReason != "" {
		compiled, err := regexp.Compile(rule.MatchReason)
		if err != nil {
			return fmt.Errorf("headermanip: invalid reason regex: %w", err)
		}
		rule.compiledReason = compiled
	}
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.rules = append(rw.rules, rule)
	return nil
}

func (rw *responseRewriter) Rules() []*ResponseRewriteRule {
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	result := make([]*ResponseRewriteRule, len(rw.rules))
	copy(result, rw.rules)
	return result
}

func (rw *responseRewriter) RewriteResponse(code int, reason string, rules []*ResponseRewriteRule) (int, string, bool) {
	for _, rule := range rules {
		// 匹配状态码
		if rule.MatchCode != 0 && rule.MatchCode != code {
			continue
		}

		// 匹配原因短语
		if rule.compiledReason != nil && !rule.compiledReason.MatchString(reason) {
			continue
		}

		// 匹配成功，应用改写
		newCode := code
		newReason := reason
		modified := false

		if rule.NewCode > 0 {
			newCode = rule.NewCode
			modified = true
		}
		if rule.NewReason != "" {
			newReason = rule.NewReason
			modified = true
		}

		return newCode, newReason, modified
	}

	return code, reason, false
}

// NewManipulator 创建头域操作器。
func NewManipulator() Manipulator {
	return &manipulator{}
}

func (m *manipulator) ApplyRules(req *message.Request, rules []*Rule) error {
	if req == nil || req.Headers == nil {
		return fmt.Errorf("headermanip: nil request or headers")
	}

	for _, rule := range rules {
		// 检查条件
		if rule.Condition != "" {
			matched, err := m.evaluateCondition(rule.Condition, req)
			if err != nil {
				continue
			}
			if !matched {
				continue
			}
		}

		switch rule.Action {
		case ActionAdd:
			value, err := m.EvaluateTemplate(rule.Value, req)
			if err != nil {
				continue
			}
			req.Headers.Add(rule.Name, value)

		case ActionSet:
			value, err := m.EvaluateTemplate(rule.Value, req)
			if err != nil {
				continue
			}
			req.Headers.Set(rule.Name, value)

		case ActionRemove:
			req.Headers.Del(rule.Name)

		case ActionCopy:
			if err := m.CopyHeader(req.Headers, rule.SourceHeader, rule.Name); err != nil {
				continue
			}
		}
	}

	return nil
}

func (m *manipulator) ApplyRulesToResponse(rsp *message.Response, rules []*Rule) error {
	if rsp == nil || rsp.Headers == nil {
		return fmt.Errorf("headermanip: nil response or headers")
	}

	for _, rule := range rules {
		switch rule.Action {
		case ActionAdd:
			// 响应中模板变量有限
			rsp.Headers.Add(rule.Name, rule.Value)
		case ActionSet:
			rsp.Headers.Set(rule.Name, rule.Value)
		case ActionRemove:
			rsp.Headers.Del(rule.Name)
		case ActionCopy:
			if err := m.CopyHeader(rsp.Headers, rule.SourceHeader, rule.Name); err != nil {
				continue
			}
		}
	}

	return nil
}

func (m *manipulator) EvaluateTemplate(template string, req *message.Request) (string, error) {
	if template == "" {
		return "", nil
	}

	result := template

	// 替换模板变量
	re := regexp.MustCompile(`\$\{([^}]+)\}`)
	result = re.ReplaceAllStringFunc(result, func(match string) string {
		varName := match[2 : len(match)-1]
		value := m.resolveVariable(varName, req)
		return value
	})

	return result, nil
}

func (m *manipulator) resolveVariable(varName string, req *message.Request) string {
	parts := strings.Split(varName, ".")

	switch parts[0] {
	case "header":
		if len(parts) >= 2 {
			headerName := parts[1]
			return req.Headers.Get(headerName)
		}

	case "uri":
		if req.RequestURI != nil && len(parts) >= 2 {
			switch parts[1] {
			case "user":
				return req.RequestURI.User
			case "host":
				return req.RequestURI.Host
			case "port":
				return fmt.Sprintf("%d", req.RequestURI.Port)
			}
		}

	case "method":
		return string(req.Method)

	case "callid":
		return req.CallID()

	case "from":
		if len(parts) >= 2 {
			from := req.From()
			if from != nil {
				switch parts[1] {
				case "tag":
					return from.Tag()
				case "uri":
					if from.Address != nil {
						return from.Address.String()
					}
				case "display":
					return from.DisplayName
				}
			}
		}

	case "to":
		if len(parts) >= 2 {
			to := req.To()
			if to != nil {
				switch parts[1] {
				case "tag":
					return to.Tag()
				case "uri":
					if to.Address != nil {
						return to.Address.String()
					}
				case "display":
					return to.DisplayName
				}
			}
		}

	case "timestamp":
		return fmt.Sprintf("%d", time.Now().Unix())

	case "random":
		return generateRandom(8)
	}

	return ""
}

func (m *manipulator) CopyHeader(headers *message.Headers, from, to string) error {
	value := headers.Get(from)
	if value == "" {
		return fmt.Errorf("headermanip: source header %s not found", from)
	}
	headers.Set(to, value)
	return nil
}

func (m *manipulator) evaluateCondition(condition string, req *message.Request) (bool, error) {
	// 简单条件评估：检查头域是否存在
	// 格式：header.Name 或 header.Name=value
	if strings.HasPrefix(condition, "header.") {
		parts := strings.SplitN(condition[7:], "=", 2)
		headerName := parts[0]
		value := req.Headers.Get(headerName)

		if len(parts) == 1 {
			// 仅检查存在性
			return value != "", nil
		}
		// 检查值匹配
		return value == parts[1], nil
	}

	return false, fmt.Errorf("headermanip: unsupported condition: %s", condition)
}

func generateRandom(length int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// CommonRules 返回常用的头域操作规则示例。
func CommonRules() []*Rule {
	return []*Rule{
		// 添加 X-Custom-CallID 头域，值为 Call-ID
		{
			Action: ActionAdd,
			Name:   "X-Custom-CallID",
			Value:  "${callid}",
		},
		// 添加 X-From-User 头域，值为 From URI 的用户名
		{
			Action: ActionAdd,
			Name:   "X-From-User",
			Value:  "${uri.user}",
		},
		// 复制 From tag 到 X-Original-Tag
		{
			Action:       ActionCopy,
			Name:         "X-Original-Tag",
			SourceHeader: "From",
		},
		// 移除 Privacy 头域
		{
			Action: ActionRemove,
			Name:   "Privacy",
		},
		// 条件添加：仅当 User-Agent 包含 "Polycom" 时添加标记
		{
			Action:    ActionAdd,
			Name:      "X-Device-Type",
			Value:     "polycom",
			Condition: "header.User-Agent=Polycom",
		},
	}
}

// CommonResponseRewriteRules 返回常用的响应改写规则示例。
func CommonResponseRewriteRules() []*ResponseRewriteRule {
	return []*ResponseRewriteRule{
		// 将被叫的 486 "Busy Here" 改写为 486 "User is on another call"
		{
			MatchCode:   486,
			MatchReason: "Busy Here",
			NewReason:   "User is on another call",
		},
		// 将 600 "Busy Everywhere" 改写为 486 "User Busy"
		{
			MatchCode: 600,
			NewCode:   486,
			NewReason: "User Busy",
		},
		// 将 480 "Temporarily Unavailable" 改写为 486 "Temporarily Busy"
		{
			MatchCode: 480,
			NewReason: "Temporarily Busy",
		},
		// 通用规则：将所有包含 "Busy" 的 4xx 响应统一改写
		{
			MatchCode:   0, // 匹配所有状态码
			MatchReason: "(?i)busy",
			NewReason:   "User is currently unavailable",
		},
	}
}
