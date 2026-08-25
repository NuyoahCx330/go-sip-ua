package message

import (
	"fmt"
	"strings"
)

// Headers 表示 SIP 消息头域集合，支持多值头域。
type Headers struct {
	m map[string][]string
}

// NewHeaders 创建空的头域集合。
func NewHeaders() *Headers {
	return &Headers{m: make(map[string][]string)}
}

// canonicalHeaderKey 将头域名称规范化为首字母大写格式。
func canonicalHeaderKey(name string) string {
	name = strings.ToLower(name)
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

// Get 获取头域第一个值。
func (h *Headers) Get(name string) string {
	key := canonicalHeaderKey(name)
	vals := h.m[key]
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// GetAll 获取头域所有值。
func (h *Headers) GetAll(name string) []string {
	key := canonicalHeaderKey(name)
	return h.m[key]
}

// Set 设置头域值（覆盖已有值）。
func (h *Headers) Set(name, value string) {
	key := canonicalHeaderKey(name)
	h.m[key] = []string{value}
}

// Add 追加头域值。
func (h *Headers) Add(name, value string) {
	key := canonicalHeaderKey(name)
	h.m[key] = append(h.m[key], value)
}

// Del 删除头域。
func (h *Headers) Del(name string) {
	key := canonicalHeaderKey(name)
	delete(h.m, key)
}

// Has 检查头域是否存在。
func (h *Headers) Has(name string) bool {
	key := canonicalHeaderKey(name)
	return len(h.m[key]) > 0
}

// Names 返回所有头域名称。
func (h *Headers) Names() []string {
	names := make([]string, 0, len(h.m))
	for k := range h.m {
		names = append(names, k)
	}
	return names
}

// Len 返回头域数量。
func (h *Headers) Len() int {
	return len(h.m)
}

// Clone 返回头域的深拷贝。
func (h *Headers) Clone() *Headers {
	clone := NewHeaders()
	for k, vals := range h.m {
		newVals := make([]string, len(vals))
		copy(newVals, vals)
		clone.m[k] = newVals
	}
	return clone
}

// Write 将头域序列化为 SIP 文本格式。
func (h *Headers) Write(buf *strings.Builder) {
	for name, vals := range h.m {
		for _, v := range vals {
			buf.WriteString(name)
			buf.WriteString(": ")
			buf.WriteString(v)
			buf.WriteString("\r\n")
		}
	}
}

// 常用头域名称常量。
const (
	HdrVia             = "Via"
	HdrFrom            = "From"
	HdrTo              = "To"
	HdrCallID          = "Call-ID"
	HdrCSeq            = "CSeq"
	HdrContact         = "Contact"
	HdrMaxForwards     = "Max-Forwards"
	HdrContentType     = "Content-Type"
	HdrContentLen      = "Content-Length"
	HdrContentEncoding = "Content-Encoding"
	HdrRoute           = "Route"
	HdrRecordRoute     = "Record-Route"
	HdrExpires         = "Expires"
	HdrUserAgent       = "User-Agent"
	HdrAllow           = "Allow"
	HdrSupported       = "Supported"
	HdrRequire         = "Require"
	HdrProxyRequire    = "Proxy-Require"
	HdrWWWAuth         = "WWW-Authenticate"
	HdrAuthorization   = "Authorization"
	HdrProxyAuth       = "Proxy-Authenticate"
	HdrProxyAuthz      = "Proxy-Authorization"
	HdrSubject         = "Subject"
	HdrReferTo         = "Refer-To"
	HdrReferredBy      = "Referred-By"
	HdrEvent           = "Event"
	HdrAccept          = "Accept"
	HdrAcceptEnc       = "Accept-Encoding"
	HdrAcceptLang      = "Accept-Language"
	HdrSessionExp      = "Session-Expires"
	HdrMinSE           = "Min-SE"
)

// Request 表示 SIP 请求消息。
type Request struct {
	Method     Method
	RequestURI *URI
	SIPVersion string
	Headers    *Headers
	Body       []byte

	// 缓存的解析结果，避免重复解析
	via     []*Via
	from    *NameAddr
	to      *NameAddr
	callID  string
	cseq    *CSeq
	contact *NameAddr
}

// NewRequest 创建一个新的 SIP 请求。
func NewRequest(method Method, uri *URI) *Request {
	return &Request{
		Method:     method,
		RequestURI: uri,
		SIPVersion: "SIP/2.0",
		Headers:    NewHeaders(),
	}
}

// Via 返回解析后的 Via 头域列表。
func (r *Request) Via() []*Via {
	if r.via != nil {
		return r.via
	}
	vals := r.Headers.GetAll(HdrVia)
	r.via = make([]*Via, 0, len(vals))
	for _, v := range vals {
		if parsed, err := ParseVia(v); err == nil {
			r.via = append(r.via, parsed)
		}
	}
	return r.via
}

// From 返回解析后的 From 头域。
func (r *Request) From() *NameAddr {
	if r.from != nil {
		return r.from
	}
	val := r.Headers.Get(HdrFrom)
	if val == "" {
		return nil
	}
	r.from, _ = ParseNameAddr(val)
	return r.from
}

// To 返回解析后的 To 头域。
func (r *Request) To() *NameAddr {
	if r.to != nil {
		return r.to
	}
	val := r.Headers.Get(HdrTo)
	if val == "" {
		return nil
	}
	r.to, _ = ParseNameAddr(val)
	return r.to
}

// CallID 返回 Call-ID 头域值。
func (r *Request) CallID() string {
	if r.callID != "" {
		return r.callID
	}
	r.callID = r.Headers.Get(HdrCallID)
	return r.callID
}

// CSeq 返回解析后的 CSeq 头域。
func (r *Request) CSeq() *CSeq {
	if r.cseq != nil {
		return r.cseq
	}
	val := r.Headers.Get(HdrCSeq)
	if val == "" {
		return nil
	}
	r.cseq, _ = ParseCSeq(val)
	return r.cseq
}

// Contact 返回解析后的 Contact 头域。
func (r *Request) Contact() *NameAddr {
	if r.contact != nil {
		return r.contact
	}
	val := r.Headers.Get(HdrContact)
	if val == "" {
		return nil
	}
	r.contact, _ = ParseNameAddr(val)
	return r.contact
}

// MaxForwards 返回 Max-Forwards 值。
func (r *Request) MaxForwards() int {
	val := r.Headers.Get(HdrMaxForwards)
	if val == "" {
		return 70 // RFC 3261 默认值
	}
	n := 70
	fmt.Sscanf(val, "%d", &n)
	return n
}

// String 将请求序列化为 SIP 文本格式。
func (r *Request) String() string {
	if r == nil {
		return ""
	}
	var buf strings.Builder
	// 请求行
	buf.WriteString(string(r.Method))
	buf.WriteByte(' ')
	if r.RequestURI != nil {
		buf.WriteString(r.RequestURI.String())
	} else {
		buf.WriteString("*")
	}
	buf.WriteByte(' ')
	buf.WriteString(r.SIPVersion)
	buf.WriteString("\r\n")
	// 头域
	r.Headers.Write(&buf)
	// 空行
	buf.WriteString("\r\n")
	// 消息体
	if len(r.Body) > 0 {
		buf.Write(r.Body)
	}
	return buf.String()
}

// Bytes 返回序列化后的字节切片。
func (r *Request) Bytes() []byte {
	return []byte(r.String())
}

// IsRequest 返回 true，标识这是一个请求消息。
func (r *Request) IsRequest() bool { return true }

// IsResponse 返回 false。
func (r *Request) IsResponse() bool { return false }

// Clone 返回请求的深拷贝。
func (r *Request) Clone() *Request {
	clone := &Request{
		Method:     r.Method,
		RequestURI: r.RequestURI.Clone(),
		SIPVersion: r.SIPVersion,
		Headers:    r.Headers.Clone(),
	}
	if len(r.Body) > 0 {
		clone.Body = make([]byte, len(r.Body))
		copy(clone.Body, r.Body)
	}
	return clone
}

// Response 表示 SIP 响应消息。
type Response struct {
	SIPVersion string
	StatusCode int
	Reason     string
	Headers    *Headers
	Body       []byte

	// 缓存的解析结果
	via     []*Via
	from    *NameAddr
	to      *NameAddr
	callID  string
	cseq    *CSeq
	contact *NameAddr
}

// NewResponse 创建一个新的 SIP 响应。
func NewResponse(statusCode int, reason string) *Response {
	return &Response{
		SIPVersion: "SIP/2.0",
		StatusCode: statusCode,
		Reason:     reason,
		Headers:    NewHeaders(),
	}
}

// Via 返回解析后的 Via 头域列表。
func (r *Response) Via() []*Via {
	if r.via != nil {
		return r.via
	}
	vals := r.Headers.GetAll(HdrVia)
	r.via = make([]*Via, 0, len(vals))
	for _, v := range vals {
		if parsed, err := ParseVia(v); err == nil {
			r.via = append(r.via, parsed)
		}
	}
	return r.via
}

// From 返回解析后的 From 头域。
func (r *Response) From() *NameAddr {
	if r.from != nil {
		return r.from
	}
	val := r.Headers.Get(HdrFrom)
	if val == "" {
		return nil
	}
	r.from, _ = ParseNameAddr(val)
	return r.from
}

// To 返回解析后的 To 头域。
func (r *Response) To() *NameAddr {
	if r.to != nil {
		return r.to
	}
	val := r.Headers.Get(HdrTo)
	if val == "" {
		return nil
	}
	r.to, _ = ParseNameAddr(val)
	return r.to
}

// CallID 返回 Call-ID 头域值。
func (r *Response) CallID() string {
	if r.callID != "" {
		return r.callID
	}
	r.callID = r.Headers.Get(HdrCallID)
	return r.callID
}

// CSeq 返回解析后的 CSeq 头域。
func (r *Response) CSeq() *CSeq {
	if r.cseq != nil {
		return r.cseq
	}
	val := r.Headers.Get(HdrCSeq)
	if val == "" {
		return nil
	}
	r.cseq, _ = ParseCSeq(val)
	return r.cseq
}

// Contact 返回解析后的 Contact 头域。
func (r *Response) Contact() *NameAddr {
	if r.contact != nil {
		return r.contact
	}
	val := r.Headers.Get(HdrContact)
	if val == "" {
		return nil
	}
	r.contact, _ = ParseNameAddr(val)
	return r.contact
}

// IsProvisional 判断是否为临时响应（1xx）。
func (r *Response) IsProvisional() bool {
	return r.StatusCode >= 100 && r.StatusCode < 200
}

// IsSuccess 判断是否为成功响应（2xx）。
func (r *Response) IsSuccess() bool {
	return r.StatusCode >= 200 && r.StatusCode < 300
}

// IsRedirect 判断是否为重定向响应（3xx）。
func (r *Response) IsRedirect() bool {
	return r.StatusCode >= 300 && r.StatusCode < 400
}

// IsClientError 判断是否为客户端错误响应（4xx）。
func (r *Response) IsClientError() bool {
	return r.StatusCode >= 400 && r.StatusCode < 500
}

// IsServerError 判断是否为服务端错误响应（5xx）。
func (r *Response) IsServerError() bool {
	return r.StatusCode >= 500 && r.StatusCode < 600
}

// IsGlobalError 判断是否为全局错误响应（6xx）。
func (r *Response) IsGlobalError() bool {
	return r.StatusCode >= 600
}

// IsFinal 判断是否为最终响应（>= 200）。
func (r *Response) IsFinal() bool {
	return r.StatusCode >= 200
}

// String 将响应序列化为 SIP 文本格式。
func (r *Response) String() string {
	var buf strings.Builder
	// 状态行
	buf.WriteString(r.SIPVersion)
	buf.WriteByte(' ')
	buf.WriteString(fmt.Sprintf("%d", r.StatusCode))
	buf.WriteByte(' ')
	buf.WriteString(r.Reason)
	buf.WriteString("\r\n")
	// 头域
	r.Headers.Write(&buf)
	// 空行
	buf.WriteString("\r\n")
	// 消息体
	if len(r.Body) > 0 {
		buf.Write(r.Body)
	}
	return buf.String()
}

// Bytes 返回序列化后的字节切片。
func (r *Response) Bytes() []byte {
	return []byte(r.String())
}

// IsRequest 返回 false。
func (r *Response) IsRequest() bool { return false }

// IsResponse 返回 true，标识这是一个响应消息。
func (r *Response) IsResponse() bool { return true }

// Clone 返回响应的深拷贝。
func (r *Response) Clone() *Response {
	clone := &Response{
		SIPVersion: r.SIPVersion,
		StatusCode: r.StatusCode,
		Reason:     r.Reason,
		Headers:    r.Headers.Clone(),
	}
	if len(r.Body) > 0 {
		clone.Body = make([]byte, len(r.Body))
		copy(clone.Body, r.Body)
	}
	return clone
}

// Message 是 SIP 请求和响应的通用接口。
type Message interface {
	IsRequest() bool
	IsResponse() bool
	Headers_() *Headers
	String() string
	Bytes() []byte
}

// Headers_ 返回请求的头域（实现 Message 接口）。
func (r *Request) Headers_() *Headers { return r.Headers }

// Headers_ 返回响应的头域（实现 Message 接口）。
func (r *Response) Headers_() *Headers { return r.Headers }

// ReasonPhrase 根据状态码返回标准原因短语。
func ReasonPhrase(code int) string {
	switch code {
	case 100:
		return "Trying"
	case 180:
		return "Ringing"
	case 181:
		return "Call Is Being Forwarded"
	case 182:
		return "Queued"
	case 183:
		return "Session Progress"
	case 200:
		return "OK"
	case 202:
		return "Accepted"
	case 300:
		return "Multiple Choices"
	case 301:
		return "Moved Permanently"
	case 302:
		return "Moved Temporarily"
	case 305:
		return "Use Proxy"
	case 380:
		return "Alternative Service"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 406:
		return "Not Acceptable"
	case 407:
		return "Proxy Authentication Required"
	case 408:
		return "Request Timeout"
	case 410:
		return "Gone"
	case 413:
		return "Request Entity Too Large"
	case 414:
		return "Request-URI Too Long"
	case 415:
		return "Unsupported Media Type"
	case 416:
		return "Unsupported URI Scheme"
	case 420:
		return "Bad Extension"
	case 421:
		return "Extension Required"
	case 423:
		return "Interval Too Brief"
	case 480:
		return "Temporarily Unavailable"
	case 481:
		return "Call/Transaction Does Not Exist"
	case 482:
		return "Loop Detected"
	case 483:
		return "Too Many Hops"
	case 484:
		return "Address Incomplete"
	case 485:
		return "Ambiguous"
	case 486:
		return "Busy Here"
	case 487:
		return "Request Terminated"
	case 488:
		return "Not Acceptable Here"
	case 489:
		return "Bad Event"
	case 491:
		return "Request Pending"
	case 493:
		return "Undecipherable"
	case 500:
		return "Server Internal Error"
	case 501:
		return "Not Implemented"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	case 504:
		return "Server Time-out"
	case 505:
		return "Version Not Supported"
	case 513:
		return "Message Too Large"
	case 600:
		return "Busy Everywhere"
	case 603:
		return "Decline"
	case 604:
		return "Does Not Exist Anywhere"
	case 606:
		return "Not Acceptable"
	default:
		return "Unknown"
	}
}
