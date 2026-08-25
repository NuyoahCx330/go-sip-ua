// Package auth 实现 SIP Digest 认证（RFC 2617），支持代理认证和 UAC/UAS 双向认证。
package auth

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
)

// Algorithm 摘要算法。
type Algorithm string

const (
	AlgMD5     Algorithm = "MD5"
	AlgMD5Sess Algorithm = "MD5-sess"
)

// QOP 保护质量。
type QOP string

const (
	QOPAuth    QOP = "auth"
	QOPAuthInt QOP = "auth-int"
)

// Challenge 表示认证挑战（WWW-Authenticate / Proxy-Authenticate）。
type Challenge struct {
	Realm     string
	Nonce     string
	Opaque    string
	Stale     bool
	Algorithm Algorithm
	QOP       []QOP
	Domain    []string
}

// Credentials 表示认证凭据。
type Credentials struct {
	Username  string
	Password  string
	Realm     string
	URI       string
	Nonce     string
	Response  string
	Algorithm Algorithm
	QOP       QOP
	Nc        uint32
	CNonce    string
	Opaque    string
}

// ParseChallenge 从头域值解析认证挑战。
func ParseChallenge(header string) (*Challenge, error) {
	if !strings.HasPrefix(strings.ToLower(header), "digest ") {
		return nil, errors.New("auth: unsupported authentication scheme")
	}

	params := parseDigestParams(header[7:])
	ch := &Challenge{}

	if v, ok := params["realm"]; ok {
		ch.Realm = strings.Trim(v, "\"")
	}
	if v, ok := params["nonce"]; ok {
		ch.Nonce = strings.Trim(v, "\"")
	}
	if v, ok := params["opaque"]; ok {
		ch.Opaque = strings.Trim(v, "\"")
	}
	if v, ok := params["stale"]; ok {
		ch.Stale = strings.ToLower(v) == "true"
	}
	if v, ok := params["algorithm"]; ok {
		ch.Algorithm = Algorithm(strings.Trim(v, "\""))
	} else {
		ch.Algorithm = AlgMD5
	}
	if v, ok := params["qop"]; ok {
		for _, q := range strings.Split(v, ",") {
			q = strings.TrimSpace(q)
			switch QOP(q) {
			case QOPAuth:
				ch.QOP = append(ch.QOP, QOPAuth)
			case QOPAuthInt:
				ch.QOP = append(ch.QOP, QOPAuthInt)
			}
		}
	}
	if v, ok := params["domain"]; ok {
		for _, d := range strings.Split(v, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				ch.Domain = append(ch.Domain, d)
			}
		}
	}

	return ch, nil
}

// BuildAuthorization 根据挑战和凭据构建 Authorization 头域值。
func BuildAuthorization(ch *Challenge, cred *Credentials, method, uri string, body []byte) (string, error) {
	if ch.Realm == "" || ch.Nonce == "" {
		return "", errors.New("auth: missing realm or nonce")
	}

	cred.Realm = ch.Realm
	cred.Nonce = ch.Nonce
	cred.URI = uri
	cred.Algorithm = ch.Algorithm
	cred.Opaque = ch.Opaque

	// 选择 QOP
	if len(ch.QOP) > 0 {
		cred.QOP = ch.QOP[0]
		cred.Nc++
		if cred.CNonce == "" {
			cred.CNonce = generateCNonce()
		}
	}

	// 计算摘要响应
	response, err := computeDigest(cred, method, body)
	if err != nil {
		return "", err
	}
	cred.Response = response

	// 构建头域值
	var parts []string
	parts = append(parts, fmt.Sprintf(`username="%s"`, cred.Username))
	parts = append(parts, fmt.Sprintf(`realm="%s"`, cred.Realm))
	parts = append(parts, fmt.Sprintf(`nonce="%s"`, cred.Nonce))
	parts = append(parts, fmt.Sprintf(`uri="%s"`, cred.URI))
	parts = append(parts, fmt.Sprintf(`response="%s"`, cred.Response))

	if cred.Algorithm != "" {
		parts = append(parts, fmt.Sprintf(`algorithm=%s`, cred.Algorithm))
	}
	if cred.QOP != "" {
		parts = append(parts, fmt.Sprintf(`qop=%s`, cred.QOP))
		parts = append(parts, fmt.Sprintf(`nc=%08x`, cred.Nc))
		parts = append(parts, fmt.Sprintf(`cnonce="%s"`, cred.CNonce))
	}
	if cred.Opaque != "" {
		parts = append(parts, fmt.Sprintf(`opaque="%s"`, cred.Opaque))
	}

	return "Digest " + strings.Join(parts, ", "), nil
}

// ParseAuthorization 解析 Authorization 头域值。
func ParseAuthorization(header string) (*Credentials, error) {
	if !strings.HasPrefix(strings.ToLower(header), "digest ") {
		return nil, errors.New("auth: unsupported authorization scheme")
	}

	params := parseDigestParams(header[7:])
	cred := &Credentials{}

	if v, ok := params["username"]; ok {
		cred.Username = strings.Trim(v, "\"")
	}
	if v, ok := params["realm"]; ok {
		cred.Realm = strings.Trim(v, "\"")
	}
	if v, ok := params["nonce"]; ok {
		cred.Nonce = strings.Trim(v, "\"")
	}
	if v, ok := params["uri"]; ok {
		cred.URI = strings.Trim(v, "\"")
	}
	if v, ok := params["response"]; ok {
		cred.Response = strings.Trim(v, "\"")
	}
	if v, ok := params["algorithm"]; ok {
		cred.Algorithm = Algorithm(strings.Trim(v, "\""))
	}
	if v, ok := params["qop"]; ok {
		cred.QOP = QOP(v)
	}
	if v, ok := params["opaque"]; ok {
		cred.Opaque = strings.Trim(v, "\"")
	}
	if v, ok := params["nc"]; ok {
		fmt.Sscanf(strings.Trim(v, "\""), "%x", &cred.Nc)
	}
	if v, ok := params["cnonce"]; ok {
		cred.CNonce = strings.Trim(v, "\"")
	}

	return cred, nil
}

// VerifyAuthorization 验证客户端的认证响应。
func VerifyAuthorization(ch *Challenge, cred *Credentials, method string, body []byte, password string) (bool, error) {
	expected, err := computeDigest(&Credentials{
		Username:  cred.Username,
		Password:  password,
		Realm:     ch.Realm,
		Nonce:     ch.Nonce,
		URI:       cred.URI,
		Algorithm: cred.Algorithm,
		QOP:       cred.QOP,
		Nc:        cred.Nc,
		CNonce:    cred.CNonce,
	}, method, body)
	if err != nil {
		return false, err
	}
	return expected == cred.Response, nil
}

// HandleAuthChallenge 处理认证挑战，为请求添加 Authorization 头域。
func HandleAuthChallenge(req *message.Request, rsp *message.Response, username, password string) error {
	// 优先处理 Proxy-Authenticate
	authHeader := rsp.Headers.Get(message.HdrProxyAuth)
	targetHeader := message.HdrProxyAuthz
	if authHeader == "" {
		authHeader = rsp.Headers.Get(message.HdrWWWAuth)
		targetHeader = message.HdrAuthorization
	}
	if authHeader == "" {
		return errors.New("auth: no authentication challenge in response")
	}

	ch, err := ParseChallenge(authHeader)
	if err != nil {
		return err
	}

	cred := &Credentials{Username: username, Password: password}
	uri := req.RequestURI.String()
	authValue, err := BuildAuthorization(ch, cred, string(req.Method), uri, req.Body)
	if err != nil {
		return err
	}

	req.Headers.Set(targetHeader, authValue)
	return nil
}

// computeDigest 计算摘要响应。
func computeDigest(cred *Credentials, method string, body []byte) (string, error) {
	// HA1 = MD5(username:realm:password)
	ha1 := md5Hash(fmt.Sprintf("%s:%s:%s", cred.Username, cred.Realm, cred.Password))

	if cred.Algorithm == AlgMD5Sess {
		ha1 = md5Hash(fmt.Sprintf("%s:%s:%s", ha1, cred.Nonce, cred.CNonce))
	}

	// HA2 = MD5(method:uri)
	ha2 := md5Hash(fmt.Sprintf("%s:%s", method, cred.URI))

	if cred.QOP == QOPAuthInt && body != nil {
		hbody := md5Hash(string(body))
		ha2 = md5Hash(fmt.Sprintf("%s:%s:%s", method, cred.URI, hbody))
	}

	// Response
	var response string
	switch cred.QOP {
	case QOPAuth, QOPAuthInt:
		response = md5Hash(fmt.Sprintf("%s:%s:%08x:%s:%s:%s",
			ha1, cred.Nonce, cred.Nc, cred.CNonce, string(cred.QOP), ha2))
	default:
		response = md5Hash(fmt.Sprintf("%s:%s:%s", ha1, cred.Nonce, ha2))
	}

	return response, nil
}

func md5Hash(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func generateCNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func parseDigestParams(s string) map[string]string {
	params := make(map[string]string)
	s = strings.TrimSpace(s)

	for s != "" {
		// 跳过逗号/空格
		s = strings.TrimLeft(s, ", ")
		if s == "" {
			break
		}

		eqIdx := strings.IndexByte(s, '=')
		if eqIdx < 0 {
			break
		}

		key := strings.TrimSpace(s[:eqIdx])
		s = s[eqIdx+1:]

		var value string
		if len(s) > 0 && s[0] == '"' {
			// 引号值
			endQuote := strings.IndexByte(s[1:], '"')
			if endQuote < 0 {
				value = s[1:]
				s = ""
			} else {
				value = s[1 : endQuote+1]
				s = s[endQuote+2:]
			}
		} else {
			// 非引号值
			commaIdx := strings.IndexByte(s, ',')
			if commaIdx < 0 {
				value = s
				s = ""
			} else {
				value = s[:commaIdx]
				s = s[commaIdx:]
			}
		}

		params[strings.ToLower(key)] = value
	}

	return params
}

// NonceManager 管理服务端 Nonce 的生成和验证。
type NonceManager struct {
	nonces map[string]nonceInfo
	count  atomic.Int64
}

type nonceInfo struct {
	createdAt int64
	useCount  int32
}

// NewNonceManager 创建 Nonce 管理器。
func NewNonceManager() *NonceManager {
	return &NonceManager{
		nonces: make(map[string]nonceInfo),
	}
}

// GenerateNonce 生成新的 Nonce。
func (nm *NonceManager) GenerateNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	nonce := hex.EncodeToString(b)
	nm.nonces[nonce] = nonceInfo{createdAt: 0, useCount: 0}
	nm.count.Add(1)
	return nonce
}

// ValidateNonce 验证 Nonce 是否有效。
func (nm *NonceManager) ValidateNonce(nonce string) bool {
	_, ok := nm.nonces[nonce]
	return ok
}
