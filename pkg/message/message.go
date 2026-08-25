// Package message 提供 SIP 消息的解析、构造和头域处理能力。
// 实现遵循 RFC 3261 规范，支持完整的 SIP 请求和响应消息处理。
package message

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Method 表示 SIP 请求方法。
type Method string

const (
	INVITE    Method = "INVITE"
	ACK       Method = "ACK"
	BYE       Method = "BYE"
	CANCEL    Method = "CANCEL"
	REGISTER  Method = "REGISTER"
	OPTIONS   Method = "OPTIONS"
	SUBSCRIBE Method = "SUBSCRIBE"
	NOTIFY    Method = "NOTIFY"
	REFER     Method = "REFER"
	UPDATE    Method = "UPDATE"
	INFO      Method = "INFO"
	PRACK     Method = "PRACK"
	PUBLISH   Method = "PUBLISH"
	MESSAGE   Method = "MESSAGE"
)

// compactHeaders 紧凑头域名称到标准名称的映射（RFC 3261 Section 7.3.3）。
var compactHeaders = map[string]string{
	"i": HdrCallID,
	"m": HdrContact,
	"e": HdrContentEncoding,
	"l": HdrContentLen,
	"c": HdrContentType,
	"f": HdrFrom,
	"s": HdrSubject,
	"k": HdrSupported,
	"t": HdrTo,
	"v": HdrVia,
	"o": HdrEvent,
	"r": HdrReferTo,
	"b": HdrReferredBy,
	"u": HdrAllow,
	"n": "Identity",
}

// URI 表示 SIP URI（sip:user@host:port;params?headers）。
type URI struct {
	Scheme   string
	User     string
	Password string
	Host     string
	Port     int
	Params   Params
	Headers  map[string]string
}

// String 返回 URI 的字符串表示。
func (u *URI) String() string {
	if u == nil {
		return ""
	}
	var buf strings.Builder
	buf.WriteString(u.Scheme)
	buf.WriteByte(':')
	if u.User != "" {
		buf.WriteString(escapeUserinfo(u.User))
		if u.Password != "" {
			buf.WriteByte(':')
			buf.WriteString(u.Password)
		}
		buf.WriteByte('@')
	}
	if ip := net.ParseIP(u.Host); ip != nil && ip.To4() == nil {
		buf.WriteByte('[')
		buf.WriteString(u.Host)
		buf.WriteByte(']')
	} else {
		buf.WriteString(u.Host)
	}
	if u.Port > 0 {
		buf.WriteByte(':')
		buf.WriteString(strconv.Itoa(u.Port))
	}
	for k, v := range u.Params {
		buf.WriteByte(';')
		buf.WriteString(k)
		if v != "" {
			buf.WriteByte('=')
			buf.WriteString(v)
		}
	}
	if len(u.Headers) > 0 {
		buf.WriteByte('?')
		first := true
		for k, v := range u.Headers {
			if !first {
				buf.WriteByte('&')
			}
			buf.WriteString(k)
			buf.WriteByte('=')
			buf.WriteString(v)
			first = false
		}
	}
	return buf.String()
}

// HostPort 返回 host:port 格式的字符串。
func (u *URI) HostPort() string {
	if u.Port > 0 {
		return net.JoinHostPort(u.Host, strconv.Itoa(u.Port))
	}
	return u.Host
}

// Addr 返回包含 scheme 默认端口的 host:port。
func (u *URI) Addr() string {
	port := u.Port
	if port == 0 {
		if u.Scheme == "sips" {
			port = 5061
		} else {
			port = 5060
		}
	}
	return net.JoinHostPort(u.Host, strconv.Itoa(port))
}

// IsSecure 返回是否为安全 URI（sips）。
func (u *URI) IsSecure() bool {
	return u.Scheme == "sips"
}

// Clone 返回 URI 的深拷贝。
func (u *URI) Clone() *URI {
	clone := &URI{
		Scheme:   u.Scheme,
		User:     u.User,
		Password: u.Password,
		Host:     u.Host,
		Port:     u.Port,
	}
	if u.Params != nil {
		clone.Params = make(Params, len(u.Params))
		for k, v := range u.Params {
			clone.Params[k] = v
		}
	}
	if u.Headers != nil {
		clone.Headers = make(map[string]string, len(u.Headers))
		for k, v := range u.Headers {
			clone.Headers[k] = v
		}
	}
	return clone
}

// Equal 比较两个 URI 是否相等（RFC 3261 Section 19.1.4）。
func (u *URI) Equal(other *URI) bool {
	if u == nil || other == nil {
		return u == other
	}
	if u.Scheme != other.Scheme {
		return false
	}
	// user, password, host, port 比较（host 不区分大小写）
	if !strings.EqualFold(u.User, other.User) {
		return false
	}
	if u.Password != other.Password {
		return false
	}
	if !strings.EqualFold(u.Host, other.Host) {
		return false
	}
	// 端口：未指定时使用 scheme 默认端口
	uPort := u.Port
	if uPort == 0 {
		if u.Scheme == "sips" {
			uPort = 5061
		} else {
			uPort = 5060
		}
	}
	oPort := other.Port
	if oPort == 0 {
		if other.Scheme == "sips" {
			oPort = 5061
		} else {
			oPort = 5060
		}
	}
	if uPort != oPort {
		return false
	}
	// 参数比较（transport, user, ttl, method, maddr 参与比较）
	compareParams := []string{"transport", "user", "ttl", "method", "maddr"}
	for _, p := range compareParams {
		v1, ok1 := u.Params.Get(p)
		v2, ok2 := other.Params.Get(p)
		if ok1 != ok2 {
			return false
		}
		if ok1 && !strings.EqualFold(v1, v2) {
			return false
		}
	}
	return true
}

// Params 表示 SIP 头域或 URI 中的参数集合。
type Params map[string]string

// Get 获取参数值。
func (p Params) Get(key string) (string, bool) {
	v, ok := p[strings.ToLower(key)]
	return v, ok
}

// Set 设置参数。
func (p Params) Set(key, value string) {
	p[strings.ToLower(key)] = value
}

// Has 检查参数是否存在。
func (p Params) Has(key string) bool {
	_, ok := p[strings.ToLower(key)]
	return ok
}

// Del 删除参数。
func (p Params) Del(key string) {
	delete(p, strings.ToLower(key))
}

// Clone 返回参数的深拷贝。
func (p Params) Clone() Params {
	if p == nil {
		return nil
	}
	clone := make(Params, len(p))
	for k, v := range p {
		clone[k] = v
	}
	return clone
}

// String 将参数序列化为字符串。
func (p Params) String() string {
	var buf strings.Builder
	for k, v := range p {
		buf.WriteByte(';')
		buf.WriteString(k)
		if v != "" {
			buf.WriteByte('=')
			buf.WriteString(v)
		}
	}
	return buf.String()
}

// Address 表示 SIP 地址（显示名 + URI）。
type Address struct {
	DisplayName string
	URI         *URI
}

// String 返回地址的字符串表示。
func (a *Address) String() string {
	var buf strings.Builder
	if a.DisplayName != "" {
		buf.WriteByte('"')
		buf.WriteString(a.DisplayName)
		buf.WriteString("\" ")
	}
	buf.WriteByte('<')
	buf.WriteString(a.URI.String())
	buf.WriteByte('>')
	return buf.String()
}

// Via 表示 Via 头域。
type Via struct {
	Transport string
	Host      string
	Port      int
	Params    Params
}

// String 返回 Via 的字符串表示。
func (v *Via) String() string {
	var buf strings.Builder
	buf.WriteString("SIP/2.0/")
	buf.WriteString(v.Transport)
	buf.WriteByte(' ')
	if ip := net.ParseIP(v.Host); ip != nil && ip.To4() == nil {
		buf.WriteByte('[')
		buf.WriteString(v.Host)
		buf.WriteByte(']')
	} else {
		buf.WriteString(v.Host)
	}
	if v.Port > 0 {
		buf.WriteByte(':')
		buf.WriteString(strconv.Itoa(v.Port))
	}
	for k, val := range v.Params {
		buf.WriteByte(';')
		buf.WriteString(k)
		if val != "" {
			buf.WriteByte('=')
			buf.WriteString(val)
		}
	}
	return buf.String()
}

// SentBy 返回 Via 中的 host:port。
func (v *Via) SentBy() string {
	if v.Port > 0 {
		return net.JoinHostPort(v.Host, strconv.Itoa(v.Port))
	}
	return v.Host
}

// Branch 返回 Via 的 branch 参数。
func (v *Via) Branch() string {
	if v.Params == nil {
		return ""
	}
	b, _ := v.Params.Get("branch")
	return b
}

// IsRFC3261Branch 判断 branch 是否为 RFC 3261 格式（z9hG4bK 前缀）。
func (v *Via) IsRFC3261Branch() bool {
	return strings.HasPrefix(v.Branch(), "z9hG4bK")
}

// Clone 返回 Via 的深拷贝。
func (v *Via) Clone() *Via {
	return &Via{
		Transport: v.Transport,
		Host:      v.Host,
		Port:      v.Port,
		Params:    v.Params.Clone(),
	}
}

// NameAddr 表示 Name-Addr 头域值（To、From、Contact 等）。
type NameAddr struct {
	DisplayName string
	Address     *URI
	Params      Params
}

// String 返回 NameAddr 的字符串表示。
func (na *NameAddr) String() string {
	if na == nil || na.Address == nil {
		return ""
	}
	var buf strings.Builder
	if na.DisplayName != "" {
		buf.WriteByte('"')
		buf.WriteString(na.DisplayName)
		buf.WriteString("\" ")
	}
	buf.WriteByte('<')
	buf.WriteString(na.Address.String())
	buf.WriteByte('>')
	for k, v := range na.Params {
		buf.WriteByte(';')
		buf.WriteString(k)
		if v != "" {
			buf.WriteByte('=')
			buf.WriteString(v)
		}
	}
	return buf.String()
}

// Tag 获取 tag 参数。
func (na *NameAddr) Tag() string {
	if na.Params == nil {
		return ""
	}
	return na.Params["tag"]
}

// SetTag 设置 tag 参数。
func (na *NameAddr) SetTag(tag string) {
	if na.Params == nil {
		na.Params = make(Params)
	}
	na.Params["tag"] = tag
}

// Clone 返回 NameAddr 的深拷贝。
func (na *NameAddr) Clone() *NameAddr {
	return &NameAddr{
		DisplayName: na.DisplayName,
		Address:     na.Address.Clone(),
		Params:      na.Params.Clone(),
	}
}

// CSeq 表示 CSeq 头域。
type CSeq struct {
	SeqNo  uint32
	Method Method
}

// String 返回 CSeq 的字符串表示。
func (c *CSeq) String() string {
	return fmt.Sprintf("%d %s", c.SeqNo, c.Method)
}

// escapeUserinfo 对 URI 用户部分进行 percent-encoding。
func escapeUserinfo(s string) string {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUserUnreserved(c) {
			buf.WriteByte(c)
		} else {
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}

func isUserUnreserved(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' ||
		c == '\'' || c == '(' || c == ')'
}

// GenerateBranch 生成符合 RFC 3261 的 magic cookie 分支标识。
func GenerateBranch() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "z9hG4bK" + hex.EncodeToString(b)
}

// GenerateTag 生成 To/From 头域的 tag 参数。
func GenerateTag() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// GenerateCallID 生成 Call-ID 头域值。
func GenerateCallID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- 生产级解析器 ----

// ParseError 解析错误，包含位置信息。
type ParseError struct {
	Line    int
	Offset  int
	Message string
	Raw     string
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("sip parse error at line %d: %s (near %q)", e.Line, e.Message, e.Raw)
	}
	return fmt.Sprintf("sip parse error: %s (near %q)", e.Message, e.Raw)
}

// ParseMessage 解析原始 SIP 消息字节为 Request 或 Response。
// 完整实现 RFC 3261 Section 7 消息解析，包括：
// - 紧凑头形式支持
// - 头域折叠（continuation lines）
// - Content-Length 精确消息体提取
// - 必需头域验证
func ParseMessage(data []byte) (interface{}, error) {
	if len(data) == 0 {
		return nil, &ParseError{Message: "empty message"}
	}

	// 查找首行结束位置
	firstLineEnd := findLineEnd(data)
	if firstLineEnd < 0 {
		return nil, &ParseError{Message: "incomplete first line", Raw: safeString(data, 64)}
	}
	firstLine := string(data[:firstLineEnd])

	// 跳 CRLF
	pos := firstLineEnd
	if pos < len(data) && data[pos] == '\r' {
		pos++
	}
	if pos < len(data) && data[pos] == '\n' {
		pos++
	}

	// 判断请求还是响应
	var result interface{}
	var err error
	if strings.HasPrefix(firstLine, "SIP/2.0 ") {
		result, err = parseResponseLine(firstLine)
	} else {
		result, err = parseRequestLine(firstLine)
	}
	if err != nil {
		return nil, err
	}

	// 获取头域对象
	var headers *Headers
	switch m := result.(type) {
	case *Request:
		headers = m.Headers
	case *Response:
		headers = m.Headers
	}

	// 解析头域块
	bodyStart, err := parseHeaderBlock(data[pos:], headers)
	if err != nil {
		return nil, err
	}

	// 提取消息体
	absBodyStart := pos + bodyStart
	contentLen := getContentLength(headers)
	if contentLen > 0 && absBodyStart < len(data) {
		end := absBodyStart + contentLen
		if end > len(data) {
			end = len(data)
		}
		body := make([]byte, end-absBodyStart)
		copy(body, data[absBodyStart:end])
		switch m := result.(type) {
		case *Request:
			m.Body = body
		case *Response:
			m.Body = body
		}
	}

	return result, nil
}

// parseRequestLine 解析请求行: Method SP Request-URI SP SIP-Version
func parseRequestLine(line string) (*Request, error) {
	// 第一个空格：Method
	sp1 := strings.IndexByte(line, ' ')
	if sp1 < 0 {
		return nil, &ParseError{Message: "invalid request line: no space after method", Raw: line}
	}
	method := line[:sp1]
	if !isValidMethod(method) {
		return nil, &ParseError{Message: fmt.Sprintf("invalid method: %q", method), Raw: line}
	}

	// 第二个空格：Request-URI
	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		return nil, &ParseError{Message: "invalid request line: no space after URI", Raw: line}
	}
	uriStr := rest[:sp2]
	version := rest[sp2+1:]

	if version != "SIP/2.0" {
		return nil, &ParseError{Message: fmt.Sprintf("unsupported SIP version: %q", version), Raw: line}
	}

	uri, err := ParseURI(uriStr)
	if err != nil {
		return nil, &ParseError{Message: "invalid request URI", Raw: uriStr}
	}

	return &Request{
		Method:     Method(method),
		RequestURI: uri,
		SIPVersion: version,
		Headers:    NewHeaders(),
	}, nil
}

// parseResponseLine 解析状态行: SIP-Version SP Status-Code SP Reason-Phrase
func parseResponseLine(line string) (*Response, error) {
	sp1 := strings.IndexByte(line, ' ')
	if sp1 < 0 {
		return nil, &ParseError{Message: "invalid status line", Raw: line}
	}
	version := line[:sp1]
	if version != "SIP/2.0" {
		return nil, &ParseError{Message: fmt.Sprintf("unsupported SIP version: %q", version), Raw: line}
	}

	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		return nil, &ParseError{Message: "invalid status line: no reason phrase", Raw: line}
	}

	code, err := strconv.Atoi(rest[:sp2])
	if err != nil || code < 100 || code > 699 {
		return nil, &ParseError{Message: fmt.Sprintf("invalid status code: %q", rest[:sp2]), Raw: line}
	}

	reason := rest[sp2+1:]

	return &Response{
		SIPVersion: version,
		StatusCode: code,
		Reason:     reason,
		Headers:    NewHeaders(),
	}, nil
}

// parseHeaderBlock 解析头域块，返回消息体起始偏移。
func parseHeaderBlock(data []byte, headers *Headers) (int, error) {
	pos := 0
	lineNum := 1
	var currentName string
	var currentValue strings.Builder

	for pos < len(data) {
		// 检查空行（头域结束）
		if data[pos] == '\r' && pos+1 < len(data) && data[pos+1] == '\n' {
			// 保存最后一个头域
			if currentName != "" {
				addHeader(headers, currentName, currentValue.String())
				currentName = ""
				currentValue.Reset()
			}
			// 跳过 CRLF
			pos += 2
			return pos, nil
		}
		if data[pos] == '\n' {
			if currentName != "" {
				addHeader(headers, currentName, currentValue.String())
				currentName = ""
				currentValue.Reset()
			}
			pos++
			return pos, nil
		}

		// 查找行尾
		lineStart := pos
		lineEnd := findLineEndFrom(data, pos)
		line := string(data[lineStart:lineEnd])

		// 跳行尾
		pos = lineEnd
		if pos < len(data) && data[pos] == '\r' {
			pos++
		}
		if pos < len(data) && data[pos] == '\n' {
			pos++
		}

		// 检查是否为折叠行（以空格或 Tab 开头）
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if currentName == "" {
				return 0, &ParseError{Line: lineNum, Message: "continuation line without preceding header", Raw: line}
			}
			currentValue.WriteByte(' ')
			currentValue.WriteString(strings.TrimSpace(line))
			lineNum++
			continue
		}

		// 保存前一个头域
		if currentName != "" {
			addHeader(headers, currentName, currentValue.String())
			currentValue.Reset()
		}

		// 解析新头域 Name: Value
		colonIdx := strings.IndexByte(line, ':')
		if colonIdx < 0 {
			return 0, &ParseError{Line: lineNum, Message: "header line without colon", Raw: line}
		}

		currentName = strings.TrimSpace(line[:colonIdx])
		if currentName == "" {
			return 0, &ParseError{Line: lineNum, Message: "empty header name", Raw: line}
		}
		currentValue.WriteString(strings.TrimSpace(line[colonIdx+1:]))
		lineNum++
	}

	return pos, nil
}

// addHeader 添加头域，自动展开紧凑形式。
func addHeader(headers *Headers, name, value string) {
	// 检查紧凑形式
	if expanded, ok := compactHeaders[strings.ToLower(name)]; ok {
		name = expanded
	}
	headers.Add(name, value)
}

// getContentLength 获取 Content-Length 头域值。
func getContentLength(headers *Headers) int {
	val := headers.Get(HdrContentLen)
	if val == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil {
		return 0
	}
	return n
}

// findLineEnd 在数据中查找第一个行尾位置。
func findLineEnd(data []byte) int {
	for i := 0; i < len(data)-1; i++ {
		if data[i] == '\r' && data[i+1] == '\n' {
			return i
		}
		if data[i] == '\n' {
			return i
		}
	}
	return -1
}

// findLineEndFrom 从指定位置开始查找行尾。
func findLineEndFrom(data []byte, from int) int {
	for i := from; i < len(data); i++ {
		if data[i] == '\r' || data[i] == '\n' {
			return i
		}
	}
	return len(data)
}

func safeString(data []byte, maxLen int) string {
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "..."
}

func isValidMethod(m string) bool {
	if !utf8.ValidString(m) {
		return false
	}
	for _, c := range m {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return len(m) > 0
}

// ParseURI 解析 SIP/SIPS/TEL URI 字符串（RFC 3261 Section 19.1）。
func ParseURI(s string) (*URI, error) {
	if len(s) == 0 {
		return nil, &ParseError{Message: "empty URI"}
	}

	uri := &URI{
		Params:  make(Params),
		Headers: make(map[string]string),
	}

	// 解析 scheme
	colonIdx := strings.IndexByte(s, ':')
	if colonIdx < 0 {
		return nil, &ParseError{Message: "missing scheme in URI", Raw: s}
	}
	uri.Scheme = strings.ToLower(s[:colonIdx])
	if uri.Scheme != "sip" && uri.Scheme != "sips" && uri.Scheme != "tel" {
		return nil, &ParseError{Message: fmt.Sprintf("unsupported URI scheme: %s", uri.Scheme), Raw: s}
	}

	rest := s[colonIdx+1:]

	// 解析 URI headers（? 分隔）
	if qIdx := strings.IndexByte(rest, '?'); qIdx >= 0 {
		headerStr := rest[qIdx+1:]
		rest = rest[:qIdx]
		for _, h := range strings.Split(headerStr, "&") {
			eqIdx := strings.IndexByte(h, '=')
			if eqIdx >= 0 {
				uri.Headers[h[:eqIdx]] = h[eqIdx+1:]
			}
		}
	}

	// 解析 URI params（; 分隔）
	if semiIdx := strings.IndexByte(rest, ';'); semiIdx >= 0 {
		paramStr := rest[semiIdx+1:]
		rest = rest[:semiIdx]
		parseSemicolonParams(paramStr, uri.Params)
	}

	// 解析 user@hostport
	if atIdx := strings.IndexByte(rest, '@'); atIdx >= 0 {
		userPart := rest[:atIdx]
		rest = rest[atIdx+1:]
		if colonIdx2 := strings.IndexByte(userPart, ':'); colonIdx2 >= 0 {
			uri.User = unescapeUserinfo(userPart[:colonIdx2])
			uri.Password = userPart[colonIdx2+1:]
		} else {
			uri.User = unescapeUserinfo(userPart)
		}
	}

	// 解析 hostport
	if len(rest) > 0 && rest[0] == '[' {
		// IPv6
		bracketEnd := strings.IndexByte(rest, ']')
		if bracketEnd < 0 {
			return nil, &ParseError{Message: "invalid IPv6 address in URI", Raw: s}
		}
		uri.Host = rest[1:bracketEnd]
		rest = rest[bracketEnd+1:]
		if len(rest) > 0 && rest[0] == ':' {
			port, err := strconv.Atoi(rest[1:])
			if err != nil {
				return nil, &ParseError{Message: "invalid port in URI", Raw: s}
			}
			uri.Port = port
		}
	} else {
		if colonIdx2 := strings.LastIndexByte(rest, ':'); colonIdx2 >= 0 {
			uri.Host = rest[:colonIdx2]
			port, err := strconv.Atoi(rest[colonIdx2+1:])
			if err != nil {
				return nil, &ParseError{Message: "invalid port in URI", Raw: s}
			}
			uri.Port = port
		} else {
			uri.Host = rest
		}
	}

	if uri.Host == "" {
		return nil, &ParseError{Message: "empty host in URI", Raw: s}
	}

	return uri, nil
}

// unescapeUserinfo 解码 percent-encoded 用户信息。
func unescapeUserinfo(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			b, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err == nil {
				buf.WriteByte(byte(b))
				i += 2
				continue
			}
		}
		buf.WriteByte(s[i])
	}
	return buf.String()
}

// parseSemicolonParams 解析分号分隔的参数列表。
func parseSemicolonParams(s string, params Params) {
	for _, p := range strings.Split(s, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		eqIdx := strings.IndexByte(p, '=')
		if eqIdx >= 0 {
			params[strings.ToLower(p[:eqIdx])] = p[eqIdx+1:]
		} else {
			params[strings.ToLower(p)] = ""
		}
	}
}

// ParseVia 解析 Via 头域值（RFC 3261 Section 20.42）。
// 格式: SIP/2.0/transport host:port;params
func ParseVia(s string) (*Via, error) {
	v := &Via{Params: make(Params)}
	s = strings.TrimSpace(s)

	// 验证并跳过 "SIP/2.0/"
	upper := strings.ToUpper(s)
	if !strings.HasPrefix(upper, "SIP/2.0/") {
		return nil, &ParseError{Message: "invalid Via protocol", Raw: s}
	}
	rest := s[8:]

	// 解析 transport
	spIdx := strings.IndexByte(rest, ' ')
	if spIdx < 0 {
		return nil, &ParseError{Message: "invalid Via: missing host", Raw: s}
	}
	v.Transport = strings.ToUpper(rest[:spIdx])
	rest = strings.TrimSpace(rest[spIdx+1:])

	// 分离 hostport 和 params
	semiIdx := findParamStart(rest)
	var hostPort string
	if semiIdx >= 0 {
		hostPort = strings.TrimSpace(rest[:semiIdx])
		parseSemicolonParams(rest[semiIdx+1:], v.Params)
	} else {
		hostPort = strings.TrimSpace(rest)
	}

	// 解析 host:port
	if len(hostPort) > 0 && hostPort[0] == '[' {
		bracketEnd := strings.IndexByte(hostPort, ']')
		if bracketEnd < 0 {
			return nil, &ParseError{Message: "invalid IPv6 in Via", Raw: s}
		}
		v.Host = hostPort[1:bracketEnd]
		after := hostPort[bracketEnd+1:]
		if len(after) > 0 && after[0] == ':' {
			port, err := strconv.Atoi(after[1:])
			if err != nil {
				return nil, &ParseError{Message: "invalid port in Via", Raw: s}
			}
			v.Port = port
		}
	} else {
		if colonIdx := strings.LastIndexByte(hostPort, ':'); colonIdx >= 0 {
			v.Host = hostPort[:colonIdx]
			port, err := strconv.Atoi(hostPort[colonIdx+1:])
			if err != nil {
				return nil, &ParseError{Message: "invalid port in Via", Raw: s}
			}
			v.Port = port
		} else {
			v.Host = hostPort
		}
	}

	if v.Host == "" {
		return nil, &ParseError{Message: "empty host in Via", Raw: s}
	}

	return v, nil
}

// findParamStart 查找参数起始位置（第一个不在 hostport 中的分号）。
func findParamStart(s string) int {
	inBracket := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			inBracket = true
		case ']':
			inBracket = false
		case ';':
			if !inBracket {
				return i
			}
		}
	}
	return -1
}

// ParseNameAddr 解析 Name-Addr 格式（RFC 3261 Section 20.39）。
// 支持格式:
//
//	"Display Name" <sip:user@host>;tag=xxx
//	Display Name <sip:user@host>;tag=xxx
//	sip:user@host;tag=xxx
//	<sip:user@host>;tag=xxx
func ParseNameAddr(s string) (*NameAddr, error) {
	na := &NameAddr{
		Params: make(Params),
	}

	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return nil, &ParseError{Message: "empty NameAddr"}
	}

	ltIdx := strings.IndexByte(s, '<')
	if ltIdx >= 0 {
		// 有尖括号格式
		gtIdx := strings.IndexByte(s, '>')
		if gtIdx < 0 || gtIdx < ltIdx {
			return nil, &ParseError{Message: "missing '>' in NameAddr", Raw: s}
		}

		// 解析显示名
		displayName := strings.TrimSpace(s[:ltIdx])
		displayName = strings.Trim(displayName, "\"")
		// 解码 RFC 2047 编码字
		displayName = decodeRFC2047(displayName)
		na.DisplayName = displayName

		// 解析 URI
		uriStr := s[ltIdx+1 : gtIdx]
		uri, err := ParseURI(uriStr)
		if err != nil {
			return nil, fmt.Errorf("message: invalid URI in NameAddr: %w", err)
		}
		na.Address = uri

		// 解析 > 后的参数
		afterGt := s[gtIdx+1:]
		parseSemicolonParams(afterGt, na.Params)
	} else {
		// 无尖括号：可能是 addr-spec 或 name-addr 无尖括号形式
		semiIdx := strings.IndexByte(s, ';')
		var uriStr string
		if semiIdx >= 0 {
			uriStr = strings.TrimSpace(s[:semiIdx])
			parseSemicolonParams(s[semiIdx+1:], na.Params)
		} else {
			uriStr = s
		}
		uri, err := ParseURI(uriStr)
		if err != nil {
			return nil, fmt.Errorf("message: invalid URI in NameAddr: %w", err)
		}
		na.Address = uri
	}

	return na, nil
}

// ParseCSeq 解析 CSeq 头域值（RFC 3261 Section 20.16）。
func ParseCSeq(s string) (*CSeq, error) {
	s = strings.TrimSpace(s)
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return nil, &ParseError{Message: "invalid CSeq: expected 'number method'", Raw: s}
	}
	seqNo, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return nil, &ParseError{Message: fmt.Sprintf("invalid CSeq number: %q", parts[0]), Raw: s}
	}
	return &CSeq{
		SeqNo:  uint32(seqNo),
		Method: Method(parts[1]),
	}, nil
}

// ParseContact 解析 Contact 头域值，支持 * 通配符。
func ParseContact(s string) (*NameAddr, bool, error) {
	s = strings.TrimSpace(s)
	if s == "*" {
		return nil, true, nil
	}
	na, err := ParseNameAddr(s)
	return na, false, err
}

// ParseContacts 解析多个 Contact 头域值（逗号分隔）。
func ParseContacts(values []string) ([]*NameAddr, error) {
	var contacts []*NameAddr
	for _, v := range values {
		// 一个值中可能有多个 Contact（逗号分隔）
		for _, part := range splitRespectingAngleBrackets(v) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if part == "*" {
				continue
			}
			na, err := ParseNameAddr(part)
			if err != nil {
				continue
			}
			contacts = append(contacts, na)
		}
	}
	return contacts, nil
}

// splitRespectingAngleBrackets 在逗号处分割字符串，但忽略尖括号内的逗号。
func splitRespectingAngleBrackets(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// decodeRFC2047 解码 RFC 2047 编码字（如 =?UTF-8?B?xxx?=）。
func decodeRFC2047(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	// 简化实现：检测并剥离编码字标记
	// 完整实现需要 base64/Q 解码
	result := s
	for {
		startIdx := strings.Index(result, "=?")
		if startIdx < 0 {
			break
		}
		rest := result[startIdx+2:]
		q1 := strings.IndexByte(rest, '?')
		if q1 < 0 {
			break
		}
		// charset := rest[:q1]
		rest = rest[q1+1:]
		q2 := strings.IndexByte(rest, '?')
		if q2 < 0 {
			break
		}
		encoding := rest[:q2]
		rest = rest[q2+1:]
		endIdx := strings.Index(rest, "?=")
		if endIdx < 0 {
			break
		}
		encoded := rest[:endIdx]

		var decoded string
		switch strings.ToUpper(encoding) {
		case "B":
			// Base64 解码
			decoded = base64DecodeSimple(encoded)
		case "Q":
			// Quoted-printable 解码
			decoded = qpDecodeSimple(encoded)
		default:
			decoded = encoded
		}

		result = result[:startIdx] + decoded + rest[endIdx+2:]
	}
	return result
}

// base64DecodeSimple 使用标准库 base64 解码。
func base64DecodeSimple(s string) string {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// 尝试 RawStdEncoding（无 padding）
		decoded, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return s
		}
	}
	return string(decoded)
}

// qpDecodeSimple 简化的 quoted-printable 解码。
func qpDecodeSimple(s string) string {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			buf.WriteByte(' ')
		} else if s[i] == '=' && i+2 < len(s) {
			b, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err == nil {
				buf.WriteByte(byte(b))
				i += 2
				continue
			}
			buf.WriteByte(s[i])
		} else {
			buf.WriteByte(s[i])
		}
	}
	return buf.String()
}

// ValidateRequest 验证 SIP 请求是否包含必需头域（RFC 3261 Section 8.1.1）。
func ValidateRequest(req *Request) error {
	if req.Method == "" {
		return &ParseError{Message: "missing method"}
	}
	if req.RequestURI == nil {
		return &ParseError{Message: "missing Request-URI"}
	}
	required := []string{HdrTo, HdrFrom, HdrCallID, HdrCSeq, HdrVia, HdrMaxForwards}
	for _, h := range required {
		if !req.Headers.Has(h) {
			return &ParseError{Message: fmt.Sprintf("missing required header: %s", h)}
		}
	}
	return nil
}

// ValidateResponse 验证 SIP 响应是否包含必需头域（RFC 3261 Section 8.1.2）。
func ValidateResponse(rsp *Response) error {
	if rsp.StatusCode < 100 || rsp.StatusCode > 699 {
		return &ParseError{Message: fmt.Sprintf("invalid status code: %d", rsp.StatusCode)}
	}
	required := []string{HdrTo, HdrFrom, HdrCallID, HdrCSeq, HdrVia}
	for _, h := range required {
		if !rsp.Headers.Has(h) {
			return &ParseError{Message: fmt.Sprintf("missing required header: %s", h)}
		}
	}
	return nil
}
