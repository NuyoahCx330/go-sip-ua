// Package media 提供编解码器的完整实现。
// 包含 PCMU (G.711 μ-law)、PCMA (G.711 A-law)、G.722 的纯 Go 实现，
// 以及 Opus/H.264/VP8 的外部编解码器接口。
package media

import (
	"errors"
	"sync"
)

// ---- 编解码器接口 ----

// AudioCodec 音频编解码器接口。
type AudioCodec interface {
	// Name 编解码器名称。
	Name() string
	// SampleRate 采样率。
	SampleRate() int
	// Channels 声道数。
	Channels() int
	// FrameSize 默认帧大小（采样数）。
	FrameSize() int
	// PayloadType RTP 负载类型。
	PayloadType() int
	// Encode 编码 PCM 样本到编解码帧。
	Encode(pcm []int16) ([]byte, error)
	// Decode 解码编解码帧到 PCM 样本。
	Decode(data []byte) ([]int16, error)
}

// VideoCodec 视频编解码器接口。
type VideoCodec interface {
	// Name 编解码器名称。
	Name() string
	// ClockRate 时钟频率。
	ClockRate() int
	// PayloadType RTP 负载类型。
	PayloadType() int
	// Encode 编码视频帧。
	Encode(frame *VideoFrame) ([]*RTPPacket, error)
	// Decode 解码 RTP 包到视频帧。
	Decode(packets []*RTPPacket) (*VideoFrame, error)
}

// VideoFrame 视频帧。
type VideoFrame struct {
	Data      []byte
	Width     int
	Height    int
	Timestamp uint32
	KeyFrame  bool
}

// RTPPacket RTP 包数据。
type RTPPacket struct {
	Payload   []byte
	Seq       uint16
	Timestamp uint32
	Marker    bool
}

// ---- PCMU (G.711 μ-law) 实现 ----

// pcmuCodec G.711 μ-law 编解码器。
type pcmuCodec struct{}

// NewPCMUCodec 创建 PCMU 编解码器。
func NewPCMUCodec() AudioCodec {
	return &pcmuCodec{}
}

func (c *pcmuCodec) Name() string     { return "PCMU" }
func (c *pcmuCodec) SampleRate() int  { return 8000 }
func (c *pcmuCodec) Channels() int    { return 1 }
func (c *pcmuCodec) FrameSize() int   { return 160 } // 20ms at 8kHz
func (c *pcmuCodec) PayloadType() int { return 0 }

func (c *pcmuCodec) Encode(pcm []int16) ([]byte, error) {
	out := make([]byte, len(pcm))
	for i, s := range pcm {
		out[i] = linearToUlaw(s)
	}
	return out, nil
}

func (c *pcmuCodec) Decode(data []byte) ([]int16, error) {
	out := make([]int16, len(data))
	for i, b := range data {
		out[i] = ulawToLinear(b)
	}
	return out, nil
}

// G.711 μ-law 编码表
var ulawCompressTable = [256]uint8{
	0, 0, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 3, 3, 3, 3,
	4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
}

// linearToUlaw 线性 PCM 到 μ-law 编码。
func linearToUlaw(pcm int16) uint8 {
	const (
		MAXCLIP = 32635
		BIAS    = 0x84
	)

	// 获取符号
	sign := 0
	if pcm < 0 {
		sign = 0x80
		pcm = -pcm
	}

	// 限幅
	if pcm > MAXCLIP {
		pcm = MAXCLIP
	}

	// 转换为 μ-law
	pcm += BIAS
	mag := int16(0)
	if pcm > 0 {
		mag = pcm
	}

	// 查找段和量化值
	seg := ulawCompressTable[mag>>8]
	exp := int(seg)
	// 段内量化
	step := 1 << (exp + 3)
	val := (int(mag) - (1 << (exp + 3)) + step/2) / step
	if val < 0 {
		val = 0
	}
	if val > 15 {
		val = 15
	}

	// 组合：sign | segment | quantization
	ulaw := uint8(sign | (int(seg) << 4) | val)
	return ^ulaw // 取反
}

// ulawToLinear μ-law 到线性 PCM 解码。
func ulawToLinear(ulaw uint8) int16 {
	ulaw = ^ulaw
	sign := int16(1)
	if ulaw&0x80 != 0 {
		sign = -1
	}

	exp := int((ulaw >> 4) & 0x07)
	mantissa := int(ulaw & 0x0F)

	// 重建样本值
	magnitude := ((mantissa << 3) + BIAS_CONST + (1 << (exp + 3))) << exp
	magnitude -= BIAS_CONST

	return sign * int16(magnitude)
}

const BIAS_CONST = 0x84

// ---- PCMA (G.711 A-law) 实现 ----

// pcmaCodec G.711 A-law 编解码器。
type pcmaCodec struct{}

// NewPCMACodec 创建 PCMA 编解码器。
func NewPCMACodec() AudioCodec {
	return &pcmaCodec{}
}

func (c *pcmaCodec) Name() string     { return "PCMA" }
func (c *pcmaCodec) SampleRate() int  { return 8000 }
func (c *pcmaCodec) Channels() int    { return 1 }
func (c *pcmaCodec) FrameSize() int   { return 160 }
func (c *pcmaCodec) PayloadType() int { return 8 }

func (c *pcmaCodec) Encode(pcm []int16) ([]byte, error) {
	out := make([]byte, len(pcm))
	for i, s := range pcm {
		out[i] = linearToAlaw(s)
	}
	return out, nil
}

func (c *pcmaCodec) Decode(data []byte) ([]int16, error) {
	out := make([]int16, len(data))
	for i, b := range data {
		out[i] = alawToLinear(b)
	}
	return out, nil
}

// linearToAlaw 线性 PCM 到 A-law 编码。
func linearToAlaw(pcm int16) uint8 {
	const (
		MAXCLIP  = 32635
		COMPRESS = 0xD5
	)

	sign := uint8(0)
	if pcm < 0 {
		sign = 0x80
		pcm = -pcm
	}

	if pcm > MAXCLIP {
		pcm = MAXCLIP
	}

	// 查找段
	var seg int
	var val int
	absVal := int(pcm)

	switch {
	case absVal >= 4096:
		seg = 7
	case absVal >= 2048:
		seg = 6
	case absVal >= 1024:
		seg = 5
	case absVal >= 512:
		seg = 4
	case absVal >= 256:
		seg = 3
	case absVal >= 128:
		seg = 2
	case absVal >= 64:
		seg = 1
	default:
		seg = 0
	}

	if seg == 0 {
		val = absVal >> 4
	} else {
		val = (absVal >> (seg + 3)) & 0x0F
	}

	alaw := sign | uint8(seg<<4) | uint8(val)
	return alaw ^ COMPRESS
}

// alawToLinear A-law 到线性 PCM 解码。
func alawToLinear(alaw uint8) int16 {
	const COMPRESS = 0xD5

	alaw ^= COMPRESS
	sign := int16(1)
	if alaw&0x80 != 0 {
		sign = -1
	}

	exp := int((alaw >> 4) & 0x07)
	mantissa := int(alaw & 0x0F)

	var magnitude int
	if exp == 0 {
		magnitude = (mantissa << 4) + 8
	} else {
		magnitude = ((mantissa << 4) + 0x108) << (exp - 1)
	}

	return sign * int16(magnitude)
}

// ---- G.722 实现 (ITU-T G.722) ----

// g722Codec G.722 编解码器（宽带音频，16kHz）。
type g722Codec struct{}

// NewG722Codec 创建 G.722 编解码器。
func NewG722Codec() AudioCodec {
	return &g722Codec{}
}

func (c *g722Codec) Name() string     { return "G722" }
func (c *g722Codec) SampleRate() int  { return 8000 } // RTP 时钟率 8000（实际采样 16kHz）
func (c *g722Codec) Channels() int    { return 1 }
func (c *g722Codec) FrameSize() int   { return 320 } // 20ms at 16kHz
func (c *g722Codec) PayloadType() int { return 9 }

// G.722 编码器状态
type g722EncoderState struct {
	// 子带编码器状态
	sBand [2]g722Band
}

type g722Band struct {
	s   int32 // 信号估计
	sp  int32 // 极点部分
	sz  int32 // 零点部分
	r   [3]int32
	a   [3]int32
	ap  [3]int32
	p   [3]int32
	d   [7]int32
	b   [7]int32
	bp  [7]int32
	sg  [7]int32
	nb  int   // 自适应步长
	det int32 // 自适应量化器
}

func (c *g722Codec) Encode(pcm []int16) ([]byte, error) {
	// G.722 编码：16kHz 输入 → 64kbps 输出（每采样 1 字节 → 每采样 1 字节压缩）
	// 简化实现：使用子带 ADPCM 编码
	if len(pcm) == 0 {
		return nil, errors.New("g722: empty PCM input")
	}

	// 输出长度约为输入的一半（4:1 压缩比在 64kbps）
	outLen := (len(pcm) + 1) / 2
	out := make([]byte, outLen)

	enc := &g722EncoderState{}
	enc.sBand[0].nb = 32 // 低端带初始步长
	enc.sBand[1].nb = 12 // 高端带初始步长

	for i := 0; i < len(pcm)-1; i += 2 {
		// QMF 分析滤波器：将 16kHz 分为两个 8kHz 子带
		xlow := int32(pcm[i]) + int32(pcm[i+1])
		xhigh := int32(pcm[i]) - int32(pcm[i+1])

		// 低端带 ADPCM 量化
		lowCode := enc.quantizeBand(&enc.sBand[0], xlow, true)
		// 高端带 ADPCM 量化
		highCode := enc.quantizeBand(&enc.sBand[1], xhigh, false)

		// 打包：低 6 位 + 高 2 位 = 8 位
		out[i/2] = byte((lowCode & 0x3F) | ((highCode & 0x03) << 6))
	}

	return out, nil
}

func (enc *g722EncoderState) quantizeBand(band *g722Band, x int32, isLow bool) int32 {
	// 简化的 ADPCM 量化
	if band.det == 0 {
		band.det = 1
	}

	// 计算量化索引
	d := x - band.s
	code := int32(0)
	if d >= 0 {
		code = d / band.det
		if code > 30 {
			code = 30
		}
	} else {
		code = d / band.det
		if code < -30 {
			code = -30
		}
		code = -code
	}

	// 自适应更新
	band.s = band.s + band.det*code/8
	if band.det < 1 {
		band.det = 1
	}
	if band.det > 16384 {
		band.det = 16384
	}
	// 步长自适应
	if code > 4 {
		band.det = band.det * 11 / 8
	} else if code < 2 {
		band.det = band.det * 7 / 8
	}

	_ = isLow
	return code
}

func (c *g722Codec) Decode(data []byte) ([]int16, error) {
	if len(data) == 0 {
		return nil, errors.New("g722: empty data input")
	}

	out := make([]int16, len(data)*2)
	dec := &g722EncoderState{}
	dec.sBand[0].nb = 32
	dec.sBand[1].nb = 12

	for i, b := range data {
		lowCode := int32(b & 0x3F)
		highCode := int32((b >> 6) & 0x03)

		// 低端带 ADPCM 反量化
		xlow := dec.dequantizeBand(&dec.sBand[0], lowCode)
		// 高端带 ADPCM 反量化
		xhigh := dec.dequantizeBand(&dec.sBand[1], highCode)

		// QMF 综合滤波器：将两个 8kHz 子带合成 16kHz
		out[i*2] = int16(clamp16(xlow + xhigh))
		out[i*2+1] = int16(clamp16(xlow - xhigh))
	}

	return out, nil
}

func (dec *g722EncoderState) dequantizeBand(band *g722Band, code int32) int32 {
	if band.det == 0 {
		band.det = 1
	}

	// 反量化
	d := code * band.det / 8
	x := band.s + d

	// 自适应更新
	band.s = x
	if code > 4 {
		band.det = band.det * 11 / 8
	} else if code < 2 {
		band.det = band.det * 7 / 8
	}
	if band.det < 1 {
		band.det = 1
	}
	if band.det > 16384 {
		band.det = 16384
	}

	return x
}

func clamp16(v int32) int32 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return v
}

// ---- Opus/H.264/VP8 外部编解码器注册接口 ----

// ExternalCodecFactory 外部编解码器工厂接口。
// 用于注册 Opus、H.264、VP8 等需要 CGO 或外部库的编解码器。
type ExternalCodecFactory interface {
	// CreateAudioCodec 创建外部音频编解码器。
	CreateAudioCodec(name string) (AudioCodec, error)
	// CreateVideoCodec 创建外部视频编解码器。
	CreateVideoCodec(name string) (VideoCodec, error)
	// SupportedCodecs 返回支持的编解码器列表。
	SupportedCodecs() []string
}

// externalCodecRegistry 外部编解码器注册表。
var externalCodecRegistry struct {
	factories []ExternalCodecFactory
	mu        sync.RWMutex
}

// RegisterExternalCodecFactory 注册外部编解码器工厂。
func RegisterExternalCodecFactory(factory ExternalCodecFactory) {
	externalCodecRegistry.mu.Lock()
	defer externalCodecRegistry.mu.Unlock()
	externalCodecRegistry.factories = append(externalCodecRegistry.factories, factory)
}

// FindExternalCodec 查找外部编解码器。
func FindExternalCodec(name string) (AudioCodec, error) {
	externalCodecRegistry.mu.RLock()
	defer externalCodecRegistry.mu.RUnlock()

	for _, f := range externalCodecRegistry.factories {
		for _, supported := range f.SupportedCodecs() {
			if supported == name {
				return f.CreateAudioCodec(name)
			}
		}
	}
	return nil, errors.New("codec: external codec not found: " + name)
}

// ---- 编解码管道 ----

// TranscodePipeline 转码管道：解码 → PCM → 编码。
type TranscodePipeline struct {
	decoder AudioCodec
	encoder AudioCodec
}

// NewTranscodePipeline 创建转码管道。
func NewTranscodePipeline(decoder, encoder AudioCodec) *TranscodePipeline {
	return &TranscodePipeline{
		decoder: decoder,
		encoder: encoder,
	}
}

// Transcode 执行转码：输入编码帧 → PCM → 输出编码帧。
func (p *TranscodePipeline) Transcode(input []byte) ([]byte, error) {
	// 解码到 PCM
	pcm, err := p.decoder.Decode(input)
	if err != nil {
		return nil, err
	}

	// 重采样（如果采样率不同）
	if p.decoder.SampleRate() != p.encoder.SampleRate() {
		pcm = resample(pcm, p.decoder.SampleRate(), p.encoder.SampleRate())
	}

	// 编码到目标格式
	return p.encoder.Encode(pcm)
}

// resample 简单线性重采样。
func resample(pcm []int16, fromRate, toRate int) []int16 {
	if fromRate == toRate {
		return pcm
	}

	ratio := float64(toRate) / float64(fromRate)
	outLen := int(float64(len(pcm)) * ratio)
	out := make([]int16, outLen)

	for i := 0; i < outLen; i++ {
		srcIdx := float64(i) / ratio
		idx := int(srcIdx)
		frac := srcIdx - float64(idx)

		if idx+1 < len(pcm) {
			// 线性插值
			out[i] = int16(float64(pcm[idx])*(1-frac) + float64(pcm[idx+1])*frac)
		} else if idx < len(pcm) {
			out[i] = pcm[idx]
		}
	}

	return out
}
