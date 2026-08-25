// Package media 提供媒体处理核心功能，包括 SDP 协商、RTP 引擎、编解码管理和 SRTP 加密。
package media

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
	"github.com/NuyoahCx330/go-sip-ua/pkg/message"
)

// MediaMode 媒体处理模式。
type MediaMode int

const (
	// MediaModeSignalingOnly 仅信令模式，不处理媒体。
	MediaModeSignalingOnly MediaMode = iota
	// MediaModeRelay 媒体转发模式，不编解码。
	MediaModeRelay
	// MediaModeTranscode 编解码处理模式。
	MediaModeTranscode
)

// String 返回媒体模式的可读名称。
func (m MediaMode) String() string {
	switch m {
	case MediaModeSignalingOnly:
		return "SignalingOnly"
	case MediaModeRelay:
		return "Relay"
	case MediaModeTranscode:
		return "Transcode"
	default:
		return "Unknown"
	}
}

// SignalingOnlyConfig 仅信令模式配置。
type SignalingOnlyConfig struct {
	// Enable 启用仅信令模式。
	Enable bool
	// StripMedia 从 SDP 中移除媒体描述。
	StripMedia bool
	// RejectMediaRequests 拒绝包含 SDP 的请求。
	RejectMediaRequests bool
}

// RelayConfig 媒体转发模式配置。
type RelayConfig struct {
	// Enable 启用媒体转发模式。
	Enable bool
	// BufferSize 转发缓冲区大小（字节）。
	BufferSize int
	// EnableRTCP 是否转发 RTCP。
	EnableRTCP bool
	// EnableFork 是否启用流复制。
	EnableFork bool
	// SSRCRewrite 是否重写 SSRC。
	SSRCRewrite bool
	// TimestampRewrite 是否重写时间戳。
	TimestampRewrite bool
}

// TranscodeConfig 编解码处理模式配置。
type TranscodeConfig struct {
	// Enable 启用编解码处理模式。
	Enable bool
	// InputCodecs 输入端支持的编解码器。
	InputCodecs []string
	// OutputCodecs 输出端支持的编解码器。
	OutputCodecs []string
	// SampleRate 目标采样率。
	SampleRate int
	// Channels 目标声道数。
	Channels int
	// FrameSize 目标帧大小。
	FrameSize int
	// EnableVAD 启用语音活动检测。
	EnableVAD bool
	// EnableComfortNoise 启用舒适噪声生成。
	EnableComfortNoise bool
}

// Codec 表示一个媒体编解码器。
type Codec struct {
	Name        string
	PayloadType int
	ClockRate   int
	Channels    int    // 音频通道数
	SDPFmtp     string // fmtp 参数
}

// String 返回编解码器的可读表示。
func (c *Codec) String() string {
	return fmt.Sprintf("%s/%d", c.Name, c.ClockRate)
}

// StandardCodecs 返回标准编解码器列表。
func StandardCodecs() []*Codec {
	return []*Codec{
		{Name: "PCMU", PayloadType: 0, ClockRate: 8000, Channels: 1},
		{Name: "PCMA", PayloadType: 8, ClockRate: 8000, Channels: 1},
		{Name: "G722", PayloadType: 9, ClockRate: 8000, Channels: 1},
		{Name: "opus", PayloadType: 111, ClockRate: 48000, Channels: 2, SDPFmtp: "minptime=10;useinbandfec=1"},
		{Name: "H264", PayloadType: 96, ClockRate: 90000, Channels: 0, SDPFmtp: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		{Name: "VP8", PayloadType: 100, ClockRate: 90000, Channels: 0},
	}
}

// CodecManager 编解码管理器接口。
type CodecManager interface {
	Register(codec *Codec) error
	FindByPT(pt int) *Codec
	FindByName(name string) *Codec
	GetClockRate(codec *Codec) int
	GetSampleBits(codec *Codec) int
	GetFrameSize(codec *Codec) int
	SerializeRtpmap(codecs []*Codec) string
	Negotiate(offer []*Codec, local []*Codec) []*Codec
	CodecList() []*Codec
	PreWarm() error
}

// codecManager 是 CodecManager 的默认实现。
type codecManager struct {
	codecs []*Codec
	mu     sync.RWMutex
}

// NewCodecManager 创建编解码管理器。
func NewCodecManager() CodecManager {
	return &codecManager{}
}

func (cm *codecManager) Register(codec *Codec) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.codecs = append(cm.codecs, codec)
	return nil
}

func (cm *codecManager) FindByPT(pt int) *Codec {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for _, c := range cm.codecs {
		if c.PayloadType == pt {
			return c
		}
	}
	return nil
}

func (cm *codecManager) FindByName(name string) *Codec {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for _, c := range cm.codecs {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (cm *codecManager) Negotiate(offer []*Codec, local []*Codec) []*Codec {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	var matched []*Codec
	for _, oc := range offer {
		for _, lc := range local {
			if oc.Name == lc.Name && oc.ClockRate == lc.ClockRate {
				matched = append(matched, lc)
				break
			}
		}
	}
	return matched
}

func (cm *codecManager) CodecList() []*Codec {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	result := make([]*Codec, len(cm.codecs))
	copy(result, cm.codecs)
	return result
}

func (cm *codecManager) PreWarm() error {
	return nil
}

func (cm *codecManager) GetClockRate(codec *Codec) int {
	if codec == nil {
		return 0
	}
	return codec.ClockRate
}

func (cm *codecManager) GetSampleBits(codec *Codec) int {
	if codec == nil {
		return 0
	}
	switch codec.Name {
	case "PCMU", "PCMA":
		return 8
	case "G722":
		return 16
	case "opus":
		return 16
	default:
		return 16
	}
}

func (cm *codecManager) GetFrameSize(codec *Codec) int {
	if codec == nil {
		return 0
	}
	switch codec.Name {
	case "PCMU", "PCMA":
		return 160 // 20ms at 8kHz
	case "G722":
		return 320 // 20ms at 16kHz
	case "opus":
		return 960 // 20ms at 48kHz
	default:
		return 160
	}
}

func (cm *codecManager) SerializeRtpmap(codecs []*Codec) string {
	var parts []string
	for _, c := range codecs {
		if c == nil {
			continue
		}
		rtpmap := fmt.Sprintf("%d %s/%d", c.PayloadType, c.Name, c.ClockRate)
		if c.Channels > 1 {
			rtpmap += fmt.Sprintf("/%d", c.Channels)
		}
		parts = append(parts, rtpmap)
	}
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += "\r\n"
		}
		result += p
	}
	return result
}

// SDPNegotiator SDP 协商器接口。
type SDPNegotiator interface {
	Parse(sdpText []byte) (*message.SDP, error)
	Build(param *SDPParam) ([]byte, error)
	Negotiate(offer *message.SDP, caps *MediaCapabilities) (*message.SDP, error)
	Validate(sdp *message.SDP) error
}

// SDPParam SDP 构建参数。
type SDPParam struct {
	SessionName string
	Connection  *message.SDPConnection
	Codecs      []*Codec
	MediaTypes  []string
	LocalAddr   string
	Bandwidth   int
	Attributes  []message.SDPAttribute
}

// MediaCapabilities 媒体能力描述。
type MediaCapabilities struct {
	Codecs     []*Codec
	MediaTypes []string
}

// sdpNegotiator 是 SDPNegotiator 的默认实现。
type sdpNegotiator struct {
	cm CodecManager
}

// NewSDPNegotiator 创建 SDP 协商器。
func NewSDPNegotiator(cm CodecManager) SDPNegotiator {
	return &sdpNegotiator{cm: cm}
}

func (n *sdpNegotiator) Parse(sdpText []byte) (*message.SDP, error) {
	return message.ParseSDP(sdpText)
}

func (n *sdpNegotiator) Build(param *SDPParam) ([]byte, error) {
	sdp := &message.SDP{
		Version: 0,
		Origin: message.SDPOrigin{
			Username:       "-",
			SessionID:      rand.Uint64(),
			SessionVersion: 1,
			NetType:        "IN",
			AddrType:       "IP4",
			Address:        param.LocalAddr,
		},
		SessionName: param.SessionName,
		Connection:  param.Connection,
	}

	if sdp.SessionName == "" {
		sdp.SessionName = "-"
	}

	for _, mt := range param.MediaTypes {
		media := message.SDPMedia{
			Type:     mt,
			Port:     0,
			Protocol: "RTP/AVP",
		}

		for _, codec := range param.Codecs {
			media.Formats = append(media.Formats, fmt.Sprintf("%d", codec.PayloadType))
			media.Attributes = append(media.Attributes, message.SDPAttribute{
				Name:  "rtpmap",
				Value: fmt.Sprintf("%d %s/%d", codec.PayloadType, codec.Name, codec.ClockRate),
			})
			if codec.SDPFmtp != "" {
				media.Attributes = append(media.Attributes, message.SDPAttribute{
					Name:  "fmtp",
					Value: fmt.Sprintf("%d %s", codec.PayloadType, codec.SDPFmtp),
				})
			}
		}

		sdp.Media = append(sdp.Media, media)
	}

	return message.BuildSDP(sdp), nil
}

func (n *sdpNegotiator) Negotiate(offer *message.SDP, caps *MediaCapabilities) (*message.SDP, error) {
	if len(offer.Media) == 0 {
		return nil, errors.New("media: offer has no media sections")
	}

	answer := &message.SDP{
		Version:     offer.Version,
		Origin:      offer.Origin,
		SessionName: offer.SessionName,
		Connection:  offer.Connection,
	}

	for _, om := range offer.Media {
		var offerCodecs []*Codec
		for _, pt := range om.Formats {
			ptInt := 0
			fmt.Sscanf(pt, "%d", &ptInt)
			if c := n.cm.FindByPT(ptInt); c != nil {
				offerCodecs = append(offerCodecs, c)
			}
		}

		matched := n.cm.Negotiate(offerCodecs, caps.Codecs)
		if len(matched) == 0 {
			continue
		}

		am := message.SDPMedia{
			Type:       om.Type,
			Port:       om.Port,
			Protocol:   om.Protocol,
			Connection: om.Connection,
		}

		for _, codec := range matched {
			am.Formats = append(am.Formats, fmt.Sprintf("%d", codec.PayloadType))
			am.Attributes = append(am.Attributes, message.SDPAttribute{
				Name:  "rtpmap",
				Value: fmt.Sprintf("%d %s/%d", codec.PayloadType, codec.Name, codec.ClockRate),
			})
		}

		answer.Media = append(answer.Media, am)
	}

	if len(answer.Media) == 0 {
		return nil, errors.New("media: no compatible codecs found")
	}

	return answer, nil
}

func (n *sdpNegotiator) Validate(sdp *message.SDP) error {
	if sdp.Origin.Username == "" {
		return errors.New("media: missing origin")
	}
	if len(sdp.Media) == 0 {
		return errors.New("media: no media sections")
	}
	return nil
}

// RTPSession RTP 会话接口。
type RTPSession interface {
	Start(ctx context.Context) error
	Stop() error
	LocalAddr() *net.UDPAddr
	RemoteAddr() *net.UDPAddr
	SetRemoteAddr(addr *net.UDPAddr)
	Send(payload []byte, pt int, seq uint16, ts uint32, ssrc uint32) error
	OnReceive(fn func(payload []byte, pt int, seq uint16, ts uint32, ssrc uint32))
	Stats() *RTPStats
}

// RTPStats RTP 会话统计。
type RTPStats struct {
	PacketsSent     atomic.Int64
	PacketsReceived atomic.Int64
	BytesSent       atomic.Int64
	BytesReceived   atomic.Int64
	PacketsLost     atomic.Int64
	Jitter          atomic.Int64
}

// RTPConfig RTP 会话配置。
type RTPConfig struct {
	LocalAddr    string
	LocalPort    int
	RemoteAddr   string
	RemotePort   int
	SSRC         uint32
	PayloadType  int
	ClockRate    int
	JitterBuffer int
}

// rtpSession 是 RTPSession 的默认实现。
type rtpSession struct {
	config  RTPConfig
	conn    *net.UDPConn
	local   *net.UDPAddr
	remote  *net.UDPAddr
	ssrc    uint32
	seq     atomic.Uint32
	stats   RTPStats
	onRecv  func(payload []byte, pt int, seq uint16, ts uint32, ssrc uint32)
	log     logger.Logger
	doneCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.RWMutex
	started bool
}

// NewRTPSession 创建新的 RTP 会话。
func NewRTPSession(cfg RTPConfig, log logger.Logger) (RTPSession, error) {
	if log == nil {
		log = logger.NopLogger()
	}

	localAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", cfg.LocalAddr, cfg.LocalPort))
	if err != nil {
		return nil, fmt.Errorf("media: invalid local address: %w", err)
	}

	s := &rtpSession{
		config: cfg,
		local:  localAddr,
		ssrc:   cfg.SSRC,
		log:    log,
		doneCh: make(chan struct{}),
	}

	if cfg.RemoteAddr != "" {
		remoteAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", cfg.RemoteAddr, cfg.RemotePort))
		if err != nil {
			return nil, fmt.Errorf("media: invalid remote address: %w", err)
		}
		s.remote = remoteAddr
	}

	if s.ssrc == 0 {
		s.ssrc = rand.Uint32()
	}

	return s, nil
}

func (s *rtpSession) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("media: RTP session already started")
	}
	s.started = true
	s.mu.Unlock()

	conn, err := net.ListenUDP("udp", s.local)
	if err != nil {
		return fmt.Errorf("media: listen UDP: %w", err)
	}
	s.conn = conn
	s.local = conn.LocalAddr().(*net.UDPAddr)

	s.wg.Add(1)
	go s.readLoop(ctx)

	s.log.Info("media", "RTP session started on %s", s.local)
	return nil
}

func (s *rtpSession) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	s.mu.Unlock()

	close(s.doneCh)
	if s.conn != nil {
		s.conn.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *rtpSession) LocalAddr() *net.UDPAddr  { return s.local }
func (s *rtpSession) RemoteAddr() *net.UDPAddr { return s.remote }

func (s *rtpSession) SetRemoteAddr(addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remote = addr
}

func (s *rtpSession) Send(payload []byte, pt int, seq uint16, ts uint32, ssrc uint32) error {
	s.mu.RLock()
	remote := s.remote
	conn := s.conn
	s.mu.RUnlock()

	if conn == nil {
		return errors.New("media: RTP session not started")
	}
	if remote == nil {
		return errors.New("media: no remote address set")
	}

	packet := buildRTPPacket(pt, seq, ts, ssrc, payload)

	_, err := conn.WriteToUDP(packet, remote)
	if err != nil {
		return fmt.Errorf("media: send RTP: %w", err)
	}

	s.stats.PacketsSent.Add(1)
	s.stats.BytesSent.Add(int64(len(packet)))
	return nil
}

func (s *rtpSession) OnReceive(fn func(payload []byte, pt int, seq uint16, ts uint32, ssrc uint32)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRecv = fn
}

func (s *rtpSession) Stats() *RTPStats {
	return &s.stats
}

func (s *rtpSession) readLoop(ctx context.Context) {
	defer s.wg.Done()
	buf := make([]byte, 65535)

	for {
		select {
		case <-s.doneCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-s.doneCh:
				return
			default:
				s.stats.PacketsLost.Add(1)
				continue
			}
		}

		if n < 12 {
			continue
		}

		s.stats.PacketsReceived.Add(1)
		s.stats.BytesReceived.Add(int64(n))

		pt, seq, ts, ssrc, payload := parseRTPPacket(buf[:n])

		s.mu.RLock()
		fn := s.onRecv
		s.mu.RUnlock()

		if fn != nil {
			fn(payload, pt, seq, ts, ssrc)
		}
	}
}

func buildRTPPacket(pt int, seq uint16, ts uint32, ssrc uint32, payload []byte) []byte {
	header := make([]byte, 12+len(payload))
	header[0] = 0x80
	header[1] = byte(pt & 0x7F)
	header[2] = byte(seq >> 8)
	header[3] = byte(seq)
	header[4] = byte(ts >> 24)
	header[5] = byte(ts >> 16)
	header[6] = byte(ts >> 8)
	header[7] = byte(ts)
	header[8] = byte(ssrc >> 24)
	header[9] = byte(ssrc >> 16)
	header[10] = byte(ssrc >> 8)
	header[11] = byte(ssrc)
	copy(header[12:], payload)
	return header
}

func parseRTPPacket(data []byte) (pt int, seq uint16, ts uint32, ssrc uint32, payload []byte) {
	if len(data) < 12 {
		return
	}
	pt = int(data[1] & 0x7F)
	seq = uint16(data[2])<<8 | uint16(data[3])
	ts = uint32(data[4])<<24 | uint32(data[5])<<16 | uint32(data[6])<<8 | uint32(data[7])
	ssrc = uint32(data[8])<<24 | uint32(data[9])<<16 | uint32(data[10])<<8 | uint32(data[11])

	cc := int(data[0] & 0x0F)
	offset := 12 + cc*4

	if data[0]&0x10 != 0 && len(data) >= offset+4 {
		extLen := int(data[offset+2])<<8 | int(data[offset+3])
		offset += 4 + extLen*4
	}

	if offset < len(data) {
		payload = data[offset:]
	}
	return
}

// SRTPParam SRTP 参数。
type SRTPParam struct {
	CryptoSuite string
	Key         []byte
	Salt        []byte
	SSRC        uint32
	Roc         uint32
}

// SRTPEngine SRTP 加密引擎接口。
type SRTPEngine interface {
	Encrypt(session SRTPSession, packet []byte) ([]byte, error)
	Decrypt(session SRTPSession, packet []byte) ([]byte, error)
	EncryptRTCP(session SRTPSession, packet []byte) ([]byte, error)
	DecryptRTCP(session SRTPSession, packet []byte) ([]byte, error)
}

// SRTPSession SRTP 会话。
type SRTPSession struct {
	SendKey     []byte
	SendSalt    []byte
	RecvKey     []byte
	RecvSalt    []byte
	CryptoSuite string
}

// MediaEngine 媒体引擎，整合编解码、RTP 和 SRTP。
type MediaEngine interface {
	CodecManager() CodecManager
	Negotiator() SDPNegotiator
	CreateSession(cfg RTPConfig) (RTPSession, error)
}

// mediaEngine 是 MediaEngine 的默认实现。
type mediaEngine struct {
	cm  CodecManager
	neg SDPNegotiator
	log logger.Logger
}

// NewMediaEngine 创建媒体引擎。
func NewMediaEngine(log logger.Logger) MediaEngine {
	cm := NewCodecManager()
	for _, c := range StandardCodecs() {
		cm.Register(c)
	}
	return &mediaEngine{
		cm:  cm,
		neg: NewSDPNegotiator(cm),
		log: log,
	}
}

func (e *mediaEngine) CodecManager() CodecManager { return e.cm }
func (e *mediaEngine) Negotiator() SDPNegotiator  { return e.neg }

func (e *mediaEngine) CreateSession(cfg RTPConfig) (RTPSession, error) {
	return NewRTPSession(cfg, e.log)
}
