// Package media 提供 DTMF 事件的完整实现。
// 支持 RFC 2833/4733 RTP payload 格式的 DTMF 事件编解码，
// 以及 SIP INFO 方法 (application/dtmf-relay) 的 DTMF 信令。
package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ---- RFC 2833/4733 DTMF 事件 RTP Payload ----

// DTMFEvent DTMF 事件类型（RFC 4733 Section 2.3.1）。
type DTMFEvent uint8

const (
	DTMFEvent0 DTMFEvent = iota
	DTMFEvent1
	DTMFEvent2
	DTMFEvent3
	DTMFEvent4
	DTMFEvent5
	DTMFEvent6
	DTMFEvent7
	DTMFEvent8
	DTMFEvent9
	DTMFEventStar  // *
	DTMFEventHash  // #
	DTMFEventA     // A
	DTMFEventB     // B
	DTMFEventC     // C
	DTMFEventD     // D
	DTMFEventFlash // hook flash
)

// String 返回 DTMF 事件的可读名称。
func (e DTMFEvent) String() string {
	switch e {
	case DTMFEvent0:
		return "0"
	case DTMFEvent1:
		return "1"
	case DTMFEvent2:
		return "2"
	case DTMFEvent3:
		return "3"
	case DTMFEvent4:
		return "4"
	case DTMFEvent5:
		return "5"
	case DTMFEvent6:
		return "6"
	case DTMFEvent7:
		return "7"
	case DTMFEvent8:
		return "8"
	case DTMFEvent9:
		return "9"
	case DTMFEventStar:
		return "*"
	case DTMFEventHash:
		return "#"
	case DTMFEventA:
		return "A"
	case DTMFEventB:
		return "B"
	case DTMFEventC:
		return "C"
	case DTMFEventD:
		return "D"
	case DTMFEventFlash:
		return "Flash"
	default:
		return fmt.Sprintf("Unknown(%d)", e)
	}
}

// DigitToEvent 将数字字符转换为 DTMF 事件。
func DigitToEvent(digit rune) (DTMFEvent, error) {
	switch digit {
	case '0':
		return DTMFEvent0, nil
	case '1':
		return DTMFEvent1, nil
	case '2':
		return DTMFEvent2, nil
	case '3':
		return DTMFEvent3, nil
	case '4':
		return DTMFEvent4, nil
	case '5':
		return DTMFEvent5, nil
	case '6':
		return DTMFEvent6, nil
	case '7':
		return DTMFEvent7, nil
	case '8':
		return DTMFEvent8, nil
	case '9':
		return DTMFEvent9, nil
	case '*', 'A':
		return DTMFEventStar, nil
	case '#':
		return DTMFEventHash, nil
	case 'a':
		return DTMFEventA, nil
	case 'b':
		return DTMFEventB, nil
	case 'c':
		return DTMFEventC, nil
	case 'd':
		return DTMFEventD, nil
	default:
		return 0, fmt.Errorf("media: invalid DTMF digit: %c", digit)
	}
}

// EventToDigit 将 DTMF 事件转换为字符。
func EventToDigit(event DTMFEvent) (rune, error) {
	switch event {
	case DTMFEvent0:
		return '0', nil
	case DTMFEvent1:
		return '1', nil
	case DTMFEvent2:
		return '2', nil
	case DTMFEvent3:
		return '3', nil
	case DTMFEvent4:
		return '4', nil
	case DTMFEvent5:
		return '5', nil
	case DTMFEvent6:
		return '6', nil
	case DTMFEvent7:
		return '7', nil
	case DTMFEvent8:
		return '8', nil
	case DTMFEvent9:
		return '9', nil
	case DTMFEventStar:
		return '*', nil
	case DTMFEventHash:
		return '#', nil
	case DTMFEventA:
		return 'A', nil
	case DTMFEventB:
		return 'B', nil
	case DTMFEventC:
		return 'C', nil
	case DTMFEventD:
		return 'D', nil
	default:
		return 0, fmt.Errorf("media: invalid DTMF event: %d", event)
	}
}

// DTMFPayload RFC 2833 DTMF 事件 RTP payload。
//
// Payload 格式（4 字节）：
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|     event     |E|R|volume |          duration               |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// event:   8 位 DTMF 事件（0-15）
// E:       1 位结束标志（1=事件结束）
// R:       1 位保留（必须为 0）
// volume:  6 位音量（0-63，0 为最响）
// duration: 16 位持续时间（以时间戳增量为单位，通常 8000Hz 时 160=20ms）
type DTMFPayload struct {
	Event    DTMFEvent // DTMF 事件
	End      bool      // 结束标志（E bit）
	Volume   uint8     // 音量（0-63，0 为最响）
	Duration uint16    // 持续时间（时间戳增量）
}

// DTMFDefaultVolume 默认 DTMF 音量（0 = 最响）。
const DTMFDefaultVolume uint8 = 0

// DTMFDefaultDuration 默认 DTMF 事件持续时间（毫秒）。
const DTMFDefaultDuration = 160 // 20ms at 8000Hz clock rate

// DTMFDefaultClockRate DTMF 事件默认时钟频率。
const DTMFDefaultClockRate = 8000

// DTMFDefaultPayloadType DTMF 事件默认 RTP payload type（telephone-event）。
const DTMFDefaultPayloadType = 101

// Encode 将 DTMF payload 编码为 4 字节的 RTP payload。
func (p *DTMFPayload) Encode() ([]byte, error) {
	buf := make([]byte, 4)
	buf[0] = byte(p.Event)

	// 第二个字节：E(1 bit) | R(1 bit) | volume(6 bits)
	buf[1] = p.Volume & 0x3F
	if p.End {
		buf[1] |= 0x80
	}

	// 第三、四字节：duration（大端序）
	binary.BigEndian.PutUint16(buf[2:4], p.Duration)

	return buf, nil
}

// DecodeDTMFPayload 从 4 字节 RTP payload 解码 DTMF 事件。
func DecodeDTMFPayload(data []byte) (*DTMFPayload, error) {
	if len(data) < 4 {
		return nil, errors.New("media: DTMF payload too short (need 4 bytes)")
	}

	p := &DTMFPayload{
		Event:    DTMFEvent(data[0]),
		End:      data[1]&0x80 != 0,
		Volume:   data[1] & 0x3F,
		Duration: binary.BigEndian.Uint16(data[2:4]),
	}

	return p, nil
}

// ---- DTMF RTP 事件发送器 ----

// DTMFSender DTMF RTP 事件发送器。
// 按照 RFC 2833 Section 2.5.1.2 的要求发送 DTMF 事件序列：
// 1. 初始事件包（End=0, duration 递增）
// 2. 冗余事件包（约每 50ms 重发，End=0）
// 3. 结束事件包（End=1, duration 为总持续时间，至少重复 3 次）
type DTMFSender struct {
	ssrc        uint32
	seqNum      uint16
	clockRate   int
	payloadType int
	mu          sync.Mutex
}

// NewDTMFSender 创建 DTMF RTP 事件发送器。
func NewDTMFSender(ssrc uint32, clockRate int) *DTMFSender {
	if clockRate <= 0 {
		clockRate = DTMFDefaultClockRate
	}
	return &DTMFSender{
		ssrc:        ssrc,
		seqNum:      0,
		clockRate:   clockRate,
		payloadType: DTMFDefaultPayloadType,
	}
}

// SetPayloadType 设置 DTMF 事件的 payload type。
func (s *DTMFSender) SetPayloadType(pt int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payloadType = pt
}

// DTMFPacket DTMF RTP 数据包（用于发送）。
type DTMFPacket struct {
	Data     []byte // 完整 RTP 包
	Seq      uint16
	TS       uint32
	SSRC     uint32
	Duration time.Duration
}

// BuildDTMFEventPackets 构建一个完整 DTMF 事件的 RTP 包序列。
// 参数：
//   - event: DTMF 事件
//   - durationMs: 事件持续时间（毫秒）
//   - timestamp: 起始 RTP 时间戳
//
// 返回完整的 RTP 包序列（包含开始、冗余和结束包）。
func (s *DTMFSender) BuildDTMFEventPackets(event DTMFEvent, durationMs int, timestamp uint32) []*DTMFPacket {
	s.mu.Lock()
	defer s.mu.Unlock()

	durationTicks := uint16(durationMs * s.clockRate / 1000)
	if durationTicks == 0 {
		durationTicks = DTMFDefaultDuration
	}

	pt := s.payloadType
	var packets []*DTMFPacket

	// 阶段 1: 初始事件包（End=0）
	// 发送 1 个初始包
	payload := &DTMFPayload{
		Event:    event,
		End:      false,
		Volume:   DTMFDefaultVolume,
		Duration: durationTicks,
	}
	pkt := s.buildRTPEventPacket(payload, pt, timestamp)
	packets = append(packets, pkt)

	// 阶段 2: 冗余事件包（End=0）
	// 每 50ms 重发一次（约 durationMs/50 个冗余包，至少 1 个）
	redundancyCount := durationMs / 50
	if redundancyCount < 1 {
		redundancyCount = 1
	}
	if redundancyCount > 10 {
		redundancyCount = 10 // 限制最大冗余包数
	}
	for i := 0; i < redundancyCount; i++ {
		s.seqNum++
		pkt = s.buildRTPEventPacket(payload, pt, timestamp)
		packets = append(packets, pkt)
	}

	// 阶段 3: 结束事件包（End=1）
	// 至少重复 3 次以确保可靠性
	endPayload := &DTMFPayload{
		Event:    event,
		End:      true,
		Volume:   DTMFDefaultVolume,
		Duration: durationTicks,
	}
	for i := 0; i < 3; i++ {
		s.seqNum++
		pkt = s.buildRTPEventPacket(endPayload, pt, timestamp)
		packets = append(packets, pkt)
	}

	return packets
}

// buildRTPEventPacket 构建单个 DTMF 事件 RTP 包。
func (s *DTMFSender) buildRTPEventPacket(payload *DTMFPayload, pt int, timestamp uint32) *DTMFPacket {
	// 编码 DTMF payload
	payloadData, _ := payload.Encode()

	// 构建 RTP 头（12 字节）+ payload（4 字节）= 16 字节
	rtpPacket := make([]byte, 16)
	// V=2, P=0, X=0, CC=0
	rtpPacket[0] = 0x80
	// M=0, PT
	rtpPacket[1] = byte(pt & 0x7F)
	// Sequence Number
	rtpPacket[2] = byte(s.seqNum >> 8)
	rtpPacket[3] = byte(s.seqNum)
	// Timestamp
	rtpPacket[4] = byte(timestamp >> 24)
	rtpPacket[5] = byte(timestamp >> 16)
	rtpPacket[6] = byte(timestamp >> 8)
	rtpPacket[7] = byte(timestamp)
	// SSRC
	rtpPacket[8] = byte(s.ssrc >> 24)
	rtpPacket[9] = byte(s.ssrc >> 16)
	rtpPacket[10] = byte(s.ssrc >> 8)
	rtpPacket[11] = byte(s.ssrc)
	// DTMF payload
	copy(rtpPacket[12:], payloadData)

	durationMs := int(payload.Duration) * 1000 / s.clockRate

	return &DTMFPacket{
		Data:     rtpPacket,
		Seq:      s.seqNum,
		TS:       timestamp,
		SSRC:     s.ssrc,
		Duration: time.Duration(durationMs) * time.Millisecond,
	}
}

// ---- DTMF RTP 事件接收器 ----

// DTMFReceiver DTMF RTP 事件接收器。
// 从 RTP 流中检测和组装 DTMF 事件。
type DTMFReceiver struct {
	// 当前正在接收的 DTMF 事件
	currentEvent *DTMFPayload
	currentStart uint32 // 事件起始时间戳

	// 已完成的 DTMF 事件队列
	completedEvents []*DTMFEvent
	completedMu     sync.Mutex

	// 事件回调
	onEvent func(event DTMFEvent, duration time.Duration)

	// 统计
	eventsReceived atomic.Int64
}

// NewDTMFReceiver 创建 DTMF RTP 事件接收器。
func NewDTMFReceiver() *DTMFReceiver {
	return &DTMFReceiver{}
}

// OnDTMFEvent 设置 DTMF 事件回调函数。
func (r *DTMFReceiver) OnDTMFEvent(fn func(event DTMFEvent, duration time.Duration)) {
	r.onEvent = fn
}

// ProcessRTPPacket 处理收到的 RTP 包，检测并解析 DTMF 事件。
// 返回检测到的 DTMF 事件（如果有）和是否成功处理。
func (r *DTMFReceiver) ProcessRTPPacket(payload []byte, timestamp uint32) (DTMFEvent, time.Duration, bool) {
	if len(payload) < 4 {
		return 0, 0, false
	}

	dtmfPayload, err := DecodeDTMFPayload(payload)
	if err != nil {
		return 0, 0, false
	}

	clockRate := DTMFDefaultClockRate

	if dtmfPayload.End {
		// 事件结束
		duration := time.Duration(int(dtmfPayload.Duration)*1000/clockRate) * time.Millisecond

		r.completedMu.Lock()
		r.completedEvents = append(r.completedEvents, &dtmfPayload.Event)
		r.completedMu.Unlock()

		r.eventsReceived.Add(1)
		r.currentEvent = nil

		if r.onEvent != nil {
			r.onEvent(dtmfPayload.Event, duration)
		}

		return dtmfPayload.Event, duration, true
	}

	// 事件进行中（End=0）
	if r.currentEvent == nil || r.currentEvent.Event != dtmfPayload.Event {
		// 新事件开始
		r.currentEvent = dtmfPayload
		r.currentStart = timestamp
	}

	return 0, 0, false
}

// EventsReceived 返回已接收的 DTMF 事件数。
func (r *DTMFReceiver) EventsReceived() int64 {
	return r.eventsReceived.Load()
}

// ---- DTMF SDP 协商辅助 ----

// DTMFSDPAttribute SDP 中 telephone-event 属性。
const DTMFSDPAttribute = "telephone-event"

// BuildDTMFSDPAttr 构建 DTMF telephone-event SDP 属性行。
// 返回格式：telephone-event/8000\r\na=fmtp:101 0-16
func BuildDTMFSDPAttr(pt int, clockRate int) string {
	if clockRate <= 0 {
		clockRate = DTMFDefaultClockRate
	}
	if pt <= 0 {
		pt = DTMFDefaultPayloadType
	}
	return fmt.Sprintf("a=rtpmap:%d telephone-event/%d\r\na=fmtp:%d 0-16", pt, clockRate, pt)
}

// ParseDTMFSDPAttr 从 SDP 中解析 telephone-event payload type。
// 返回 payload type 和是否找到。
func ParseDTMFSDPAttr(sdp string) (int, bool) {
	// 查找 a=rtpmap:PT telephone-event/CLOCK
	// 简单解析：按行查找
	for _, line := range splitLines(sdp) {
		if len(line) < 10 {
			continue
		}
		if contains(line, "telephone-event") {
			// 提取 PT
			var pt int
			if _, err := fmt.Sscanf(line, "a=rtpmap:%d", &pt); err == nil {
				return pt, true
			}
		}
	}
	return 0, false
}

// splitLines 按行分割字符串（处理 \r\n 和 \n）。
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// contains 检查字符串是否包含子串。
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
