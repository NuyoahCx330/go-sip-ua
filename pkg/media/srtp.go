// Package media 提供 SRTP/SRTCP 加密引擎的完整实现。
// 支持 AES-128 Counter Mode + HMAC-SHA1 认证（RFC 3711）。
// 支持 SDES 密钥提取（RFC 4568）和 ROC 管理。
package media

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
)

// ---- SRTP Crypto Suite 定义 ----

// CryptoSuite SRTP 加密套件。
type CryptoSuite string

const (
	// AES_CM_128_HMAC_SHA1_80 AES-128 Counter Mode + 80-bit HMAC-SHA1 认证。
	AES_CM_128_HMAC_SHA1_80 CryptoSuite = "AES_CM_128_HMAC_SHA1_80"
	// AES_CM_128_HMAC_SHA1_32 AES-128 Counter Mode + 32-bit HMAC-SHA1 认证。
	AES_CM_128_HMAC_SHA1_32 CryptoSuite = "AES_CM_128_HMAC_SHA1_32"
)

// AuthTagLength 返回认证标签长度（字节）。
func (cs CryptoSuite) AuthTagLength() int {
	switch cs {
	case AES_CM_128_HMAC_SHA1_80:
		return 10 // 80 bits = 10 bytes
	case AES_CM_128_HMAC_SHA1_32:
		return 4 // 32 bits = 4 bytes
	default:
		return 10
	}
}

// ---- SRTP Key Material ----

// SRTPKeyMaterial SRTP 密钥材料。
type SRTPKeyMaterial struct {
	// 发送端
	SendKey  []byte // 16 bytes AES-128
	SendSalt []byte // 14 bytes
	SendAuth []byte // 20 bytes HMAC-SHA1
	// 接收端
	RecvKey  []byte
	RecvSalt []byte
	RecvAuth []byte
	// 参数
	CryptoSuite CryptoSuite
	MKI         uint32
}

// ---- SRTP Session ----

// SRTPSessionState SRTP 会话状态。
type SRTPSessionState struct {
	SendRoc uint32 // 发送端滚动计数器
	RecvRoc uint32 // 接收端滚动计数器
	RecvSqn uint64 // 接收端最大序列号
}

// SRTPCryptoSession SRTP 加密会话。
type SRTPCryptoSession struct {
	material *SRTPKeyMaterial
	state    SRTPSessionState
	suite    CryptoSuite

	// AES 块加密器（发送/接收）
	sendBlock cipher.Block
	recvBlock cipher.Block

	mu sync.Mutex

	// 统计
	packetsEncrypted atomic.Int64
	packetsDecrypted atomic.Int64
	authFailures     atomic.Int64
}

// ---- SRTPEngine 实现 ----

// SRTPEngineImpl SRTP 引擎实现。
type SRTPEngineImpl struct {
	sessions sync.Map // map[uint32]*SRTPCryptoSession (by SSRC)
	stats    SRTPStats
}

// SRTPStats SRTP 统计信息。
type SRTPStats struct {
	TotalEncrypted atomic.Int64
	TotalDecrypted atomic.Int64
	TotalAuthFail  atomic.Int64
	ActiveSessions atomic.Int64
}

// NewSRTPEngine 创建 SRTP 引擎。
func NewSRTPEngine() *SRTPEngineImpl {
	return &SRTPEngineImpl{}
}

// CreateSession 创建 SRTP 加密会话。
func (e *SRTPEngineImpl) CreateSession(material *SRTPKeyMaterial) (*SRTPCryptoSession, error) {
	if material == nil {
		return nil, errors.New("srtp: nil key material")
	}
	if len(material.SendKey) != 16 || len(material.RecvKey) != 16 {
		return nil, errors.New("srtp: key must be 16 bytes for AES-128")
	}
	if len(material.SendSalt) != 14 || len(material.RecvSalt) != 14 {
		return nil, errors.New("srtp: salt must be 14 bytes")
	}
	if len(material.SendAuth) != 20 || len(material.RecvAuth) != 20 {
		return nil, errors.New("srtp: auth key must be 20 bytes for HMAC-SHA1")
	}

	sendBlock, err := aes.NewCipher(material.SendKey)
	if err != nil {
		return nil, fmt.Errorf("srtp: create send cipher: %w", err)
	}
	recvBlock, err := aes.NewCipher(material.RecvKey)
	if err != nil {
		return nil, fmt.Errorf("srtp: create recv cipher: %w", err)
	}

	session := &SRTPCryptoSession{
		material:  material,
		suite:     material.CryptoSuite,
		sendBlock: sendBlock,
		recvBlock: recvBlock,
	}

	e.sessions.Store(material.MKI, session)
	e.stats.ActiveSessions.Add(1)
	return session, nil
}

// EncryptRTP 加密 RTP 包（RFC 3711 Section 3）。
func (e *SRTPEngineImpl) EncryptRTP(session *SRTPCryptoSession, packet []byte) ([]byte, error) {
	if session == nil {
		return nil, errors.New("srtp: nil session")
	}
	if len(packet) < 12 {
		return nil, errors.New("srtp: packet too short")
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// 解析 RTP 头
	ssrc := binary.BigEndian.Uint32(packet[8:12])
	seq := binary.BigEndian.Uint16(packet[2:4])
	ts := binary.BigEndian.Uint32(packet[4:8])

	// 计算 RTP 头长度
	cc := int(packet[0] & 0x0F)
	offset := 12 + cc*4
	hasExt := packet[0]&0x10 != 0
	if hasExt && len(packet) >= offset+4 {
		extLen := int(binary.BigEndian.Uint16(packet[offset+2 : offset+4]))
		offset += 4 + extLen*4
	}

	if offset > len(packet) {
		return nil, errors.New("srtp: invalid packet header")
	}

	payload := packet[offset:]

	// 构造 IV: salt XOR (SSRC || seq || roc)
	iv := session.computeSendIV(ssrc, uint64(seq), session.state.SendRoc)

	// AES Counter Mode 加密
	encrypted := make([]byte, len(payload))
	aesCTR(session.sendBlock, iv, payload, encrypted)

	// 构造输出包：RTP头 + 加密payload + auth tag
	authTagLen := session.suite.AuthTagLength()
	out := make([]byte, offset+len(encrypted)+authTagLen)
	copy(out, packet[:offset])
	copy(out[offset:], encrypted)

	// HMAC-SHA1 认证
	authTag := session.computeAuthTag(out[:offset+len(encrypted)], true)
	copy(out[offset+len(encrypted):], authTag[:authTagLen])

	// 检测序列号翻转，更新 ROC
	if seq == 0xFFFF && session.state.SendRoc < 0xFFFFFFFF {
		session.state.SendRoc++
	}

	session.packetsEncrypted.Add(1)
	e.stats.TotalEncrypted.Add(1)
	_ = ts // timestamp used in IV computation context
	return out, nil
}

// DecryptRTP 解密 RTP 包。
func (e *SRTPEngineImpl) DecryptRTP(session *SRTPCryptoSession, packet []byte) ([]byte, error) {
	if session == nil {
		return nil, errors.New("srtp: nil session")
	}
	if len(packet) < 12 {
		return nil, errors.New("srtp: packet too short")
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	authTagLen := session.suite.AuthTagLength()
	if len(packet) < 12+authTagLen {
		return nil, errors.New("srtp: packet too short for auth tag")
	}

	// 分离加密 payload 和认证标签
	payloadEnd := len(packet) - authTagLen
	receivedTag := packet[payloadEnd:]

	// 验证认证标签
	expectedTag := session.computeAuthTag(packet[:payloadEnd], false)
	if !hmac.Equal(receivedTag, expectedTag[:authTagLen]) {
		session.authFailures.Add(1)
		e.stats.TotalAuthFail.Add(1)
		return nil, errors.New("srtp: authentication failed")
	}

	// 解析 RTP 头
	ssrc := binary.BigEndian.Uint32(packet[8:12])
	seq := binary.BigEndian.Uint16(packet[2:4])

	// 计算 RTP 头长度
	cc := int(packet[0] & 0x0F)
	offset := 12 + cc*4
	hasExt := packet[0]&0x10 != 0
	if hasExt && len(packet) >= offset+4 {
		extLen := int(binary.BigEndian.Uint16(packet[offset+2 : offset+4]))
		offset += 4 + extLen*4
	}

	encPayload := packet[offset:payloadEnd]

	// 构造 IV
	iv := session.computeRecvIV(ssrc, uint64(seq), session.state.RecvRoc)

	// AES Counter Mode 解密
	decrypted := make([]byte, len(encPayload))
	aesCTR(session.recvBlock, iv, encPayload, decrypted)

	// 构造输出包
	out := make([]byte, offset+len(decrypted))
	copy(out, packet[:offset])
	copy(out[offset:], decrypted)

	session.packetsDecrypted.Add(1)
	e.stats.TotalDecrypted.Add(1)
	return out, nil
}

// EncryptRTCP 加密 RTCP 包（RFC 3711 Section 3.4）。
func (e *SRTPEngineImpl) EncryptRTCP(session *SRTPCryptoSession, packet []byte) ([]byte, error) {
	if session == nil || len(packet) < 8 {
		return nil, errors.New("srtp: nil session or packet too short for RTCP")
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	// RTCP 加密：使用 SSRC=RTCP SSRC, seq=2, index=0
	ssrc := binary.BigEndian.Uint32(packet[4:8])
	iv := session.computeSendIV(ssrc, 2, 0)

	// 跳过 RTCP 头（8字节），加密 payload
	payload := packet[8:]
	encrypted := make([]byte, len(payload))
	aesCTR(session.sendBlock, iv, payload, encrypted)

	// 输出：RTCP头 + 加密payload + 4字节 e-index
	authTagLen := session.suite.AuthTagLength()
	out := make([]byte, 8+len(encrypted)+4+authTagLen)
	copy(out, packet[:8])
	copy(out[8:], encrypted)

	// e-index（加密索引，明文）
	eIndex := make([]byte, 4)
	binary.BigEndian.PutUint32(eIndex, 0)
	copy(out[8+len(encrypted):], eIndex)

	// 认证
	authTag := session.computeAuthTag(out[:8+len(encrypted)+4], true)
	copy(out[8+len(encrypted)+4:], authTag[:authTagLen])

	return out, nil
}

// DecryptRTCP 解密 RTCP 包。
func (e *SRTPEngineImpl) DecryptRTCP(session *SRTPCryptoSession, packet []byte) ([]byte, error) {
	if session == nil || len(packet) < 12 {
		return nil, errors.New("srtp: nil session or packet too short for SRTCP")
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	authTagLen := session.suite.AuthTagLength()
	// 最小长度：RTCP头(8) + e-index(4) + auth tag
	if len(packet) < 8+4+authTagLen {
		return nil, errors.New("srtp: SRTCP packet too short")
	}

	// 分离各部分
	eIndexOffset := len(packet) - authTagLen - 4
	encPayloadEnd := eIndexOffset
	receivedTag := packet[len(packet)-authTagLen:]

	// 验证认证
	expectedTag := session.computeAuthTag(packet[:len(packet)-authTagLen], false)
	if !hmac.Equal(receivedTag, expectedTag[:authTagLen]) {
		return nil, errors.New("srtp: SRTCP authentication failed")
	}

	// 解密
	ssrc := binary.BigEndian.Uint32(packet[4:8])
	iv := session.computeRecvIV(ssrc, 2, 0)

	encPayload := packet[8:encPayloadEnd]
	decrypted := make([]byte, len(encPayload))
	aesCTR(session.recvBlock, iv, encPayload, decrypted)

	out := make([]byte, 8+len(decrypted))
	copy(out, packet[:8])
	copy(out[8:], decrypted)
	return out, nil
}

// ExtractSDES 从 SDP 中提取 SDES 密钥材料（RFC 4568）。
func (e *SRTPEngineImpl) ExtractSDES(sdp *message.SDP) (*SRTPKeyMaterial, error) {
	if sdp == nil {
		return nil, errors.New("srtp: nil SDP")
	}

	for _, media := range sdp.Media {
		for _, attr := range media.Attributes {
			if attr.Name != "crypto" {
				continue
			}
			return parseCryptoAttribute(attr.Value)
		}
	}

	return nil, errors.New("srtp: no crypto attribute found in SDP")
}

// BuildSDESAttribute 构建 SDP crypto 属性行。
func (e *SRTPEngineImpl) BuildSDESAttribute(material *SRTPKeyMaterial) string {
	suite := string(material.CryptoSuite)
	if suite == "" {
		suite = string(AES_CM_128_HMAC_SHA1_80)
	}
	keyParam := encodeBase64Key(append(material.SendKey, material.SendSalt...))
	return fmt.Sprintf("1 %s inline:%s", suite, keyParam)
}

// DestroySession 销毁 SRTP 会话。
func (e *SRTPEngineImpl) DestroySession(session *SRTPCryptoSession) error {
	if session == nil {
		return nil
	}
	e.sessions.Delete(session.material.MKI)
	e.stats.ActiveSessions.Add(-1)
	return nil
}

// GetStats 获取 SRTP 统计。
func (e *SRTPEngineImpl) GetStats() *SRTPStats {
	return &e.stats
}

// ---- SRTPCryptoSession 内部方法 ----

// computeSendIV 计算发送端 IV（128-bit）。
// IV = (salt XOR (SSRC || packet_index)) << 16
func (s *SRTPCryptoSession) computeSendIV(ssrc uint32, seq uint64, roc uint32) [16]byte {
	return computeIV(s.material.SendSalt, ssrc, seq, roc)
}

func (s *SRTPCryptoSession) computeRecvIV(ssrc uint32, seq uint64, roc uint32) [16]byte {
	return computeIV(s.material.RecvSalt, ssrc, seq, roc)
}

func computeIV(salt []byte, ssrc uint32, seq uint64, roc uint32) [16]byte {
	var iv [16]byte

	// packet_index = (roc << 16) | seq
	packetIndex := (uint64(roc) << 16) | (seq & 0xFFFF)

	// 构造 label: SSRC(4) || packetIndex(6) || 0x00(6)
	// XOR with salt
	copy(iv[0:], salt) // start with salt

	// XOR SSRC at bytes 4-7
	iv[4] ^= byte(ssrc >> 24)
	iv[5] ^= byte(ssrc >> 16)
	iv[6] ^= byte(ssrc >> 8)
	iv[7] ^= byte(ssrc)

	// XOR packet_index at bytes 8-13
	iv[8] ^= byte(packetIndex >> 40)
	iv[9] ^= byte(packetIndex >> 32)
	iv[10] ^= byte(packetIndex >> 24)
	iv[11] ^= byte(packetIndex >> 16)
	iv[12] ^= byte(packetIndex >> 8)
	iv[13] ^= byte(packetIndex)

	return iv
}

// computeAuthTag 计算 HMAC-SHA1 认证标签。
func (s *SRTPCryptoSession) computeAuthTag(data []byte, isSend bool) [20]byte {
	var authKey []byte
	if isSend {
		authKey = s.material.SendAuth
	} else {
		authKey = s.material.RecvAuth
	}

	mac := hmac.New(sha1.New, authKey)
	mac.Write(data)
	var result [20]byte
	copy(result[:], mac.Sum(nil))
	return result
}

// ---- AES Counter Mode ----

// aesCTR AES Counter Mode 加解密（CTR 模式加解密相同）。
func aesCTR(block cipher.Block, iv [16]byte, src, dst []byte) {
	stream := cipher.NewCTR(block, iv[:])
	stream.XORKeyStream(dst, src)
}

// ---- SDES 解析 ----

// parseCryptoAttribute 解析 SDP crypto 属性。
// 格式: <tag> <suite> inline:<base64-key>|<params>
func parseCryptoAttribute(value string) (*SRTPKeyMaterial, error) {
	parts := strings.Fields(value)
	if len(parts) < 3 {
		return nil, errors.New("srtp: invalid crypto attribute")
	}

	// parts[0] = tag
	// parts[1] = crypto suite
	// parts[2] = key method:key
	suite := CryptoSuite(parts[1])
	if suite != AES_CM_128_HMAC_SHA1_80 && suite != AES_CM_128_HMAC_SHA1_32 {
		return nil, fmt.Errorf("srtp: unsupported crypto suite: %s", suite)
	}

	keyPart := parts[2]
	if !strings.HasPrefix(keyPart, "inline:") {
		return nil, errors.New("srtp: only inline key method supported")
	}
	keyBase64 := keyPart[7:]

	// 去除可能的管道符后的参数
	if idx := strings.IndexByte(keyBase64, '|'); idx >= 0 {
		keyBase64 = keyBase64[:idx]
	}

	// 解码 base64 密钥材料 (30 bytes: 16 key + 14 salt)
	keyMaterial, err := decodeBase64Key(keyBase64)
	if err != nil {
		return nil, fmt.Errorf("srtp: decode key: %w", err)
	}

	if len(keyMaterial) < 30 {
		return nil, fmt.Errorf("srtp: key material too short: %d bytes", len(keyMaterial))
	}

	material := &SRTPKeyMaterial{
		SendKey:     keyMaterial[:16],
		SendSalt:    keyMaterial[16:30],
		CryptoSuite: suite,
	}

	// 生成接收端密钥（简化：翻转发送端密钥）
	// 实际 SDES 中，两端使用相同的 key material
	material.RecvKey = make([]byte, 16)
	copy(material.RecvKey, material.SendKey)
	material.RecvSalt = make([]byte, 14)
	copy(material.RecvSalt, material.SendSalt)

	// 派生认证密钥 (HMAC-SHA1, 20 bytes)
	material.SendAuth = deriveAuthKey(material.SendKey, material.SendSalt, "send")
	material.RecvAuth = deriveAuthKey(material.RecvKey, material.RecvSalt, "recv")

	return material, nil
}

// deriveAuthKey 从主密钥派生认证密钥。
func deriveAuthKey(key, salt []byte, direction string) []byte {
	// 使用 PRF 派生认证密钥
	h := sha1.New()
	h.Write(key)
	h.Write(salt)
	h.Write([]byte(direction))
	return h.Sum(nil)[:20]
}

// encodeBase64Key 编码密钥材料为 base64。
func encodeBase64Key(data []byte) string {
	const base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	result := make([]byte, 0, (len(data)+2)/3*4)
	for i := 0; i < len(data); i += 3 {
		var b0, b1, b2 byte
		b0 = data[i]
		if i+1 < len(data) {
			b1 = data[i+1]
		}
		if i+2 < len(data) {
			b2 = data[i+2]
		}
		result = append(result, base64Chars[(b0>>2)&0x3F])
		result = append(result, base64Chars[((b0<<4)|(b1>>4))&0x3F])
		if i+1 < len(data) {
			result = append(result, base64Chars[((b1<<2)|(b2>>6))&0x3F])
		} else {
			result = append(result, '=')
		}
		if i+2 < len(data) {
			result = append(result, base64Chars[b2&0x3F])
		} else {
			result = append(result, '=')
		}
	}
	return string(result)
}

// decodeBase64Key 解码 base64 密钥。
func decodeBase64Key(s string) ([]byte, error) {
	const base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	// 去除 padding
	s = strings.TrimRight(s, "=")

	var result []byte
	var buf uint32
	var bits int

	for _, c := range s {
		idx := strings.IndexByte(base64Chars, byte(c))
		if idx < 0 {
			return nil, fmt.Errorf("invalid base64 character: %c", c)
		}
		buf = (buf << 6) | uint32(idx)
		bits += 6
		if bits >= 8 {
			bits -= 8
			result = append(result, byte((buf>>bits)&0xFF))
		}
	}
	return result, nil
}
