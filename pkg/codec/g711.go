// Package codec 提供音视频编解码器的实际实现。
// 包含 G.711 (PCMU/PCMA)、G.722 等编解码器的完整编解码逻辑。
package codec

import (
	"errors"
	"sync"
)

// Codec 编解码器接口。
type Codec interface {
	// Name 编解码器名称。
	Name() string
	// PayloadType RTP 负载类型。
	PayloadType() int
	// ClockRate 时钟频率（Hz）。
	ClockRate() int
	// Channels 声道数。
	Channels() int
	// Encode 编码 PCM 样本为压缩数据。
	Encode(pcm []int16) ([]byte, error)
	// Decode 解码压缩数据为 PCM 样本。
	Decode(data []byte) ([]int16, error)
	// FrameSize 默认帧大小（样本数）。
	FrameSize() int
	// SamplesPerPacket 每包样本数。
	SamplesPerPacket() int
}

// ---- G.711 μ-law (PCMU) 实现 ----

// μ-law 参数
const (
	muLawBias    = 0x84  // 偏置值
	muLawClip    = 32635 // 削波值
	muLawMax     = 0x7FFF
	muLawSign    = 0x80
	muLawSegment = 0x70
	muLawQuantum = 0x0F
)

// ulawCompressTable μ-law 压缩查找表（13 位线性 → 8 位 μ-law）。
// 基于 ITU-T G.711 标准。
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

// ulawDecodeTable μ-law 解码查找表（8 位 μ-law → 16 位线性 PCM）。
var ulawDecodeTable = [256]int16{
	-32124, -31100, -30076, -29052, -28028, -27004, -25980, -24956,
	-23932, -22908, -21884, -20860, -19836, -18812, -17788, -16764,
	-15996, -15484, -14972, -14460, -13948, -13436, -12924, -12412,
	-11900, -11388, -10876, -10364, -9852, -9340, -8828, -8316,
	-7932, -7676, -7420, -7164, -6908, -6652, -6396, -6140,
	-5884, -5628, -5372, -5116, -4860, -4604, -4348, -4092,
	-3900, -3772, -3644, -3516, -3388, -3260, -3132, -3004,
	-2876, -2748, -2620, -2492, -2364, -2236, -2108, -1980,
	-1884, -1820, -1756, -1692, -1628, -1564, -1500, -1436,
	-1372, -1308, -1244, -1180, -1116, -1052, -988, -924,
	-876, -844, -812, -780, -748, -716, -684, -652,
	-620, -588, -556, -524, -492, -460, -428, -396,
	-372, -356, -340, -324, -308, -292, -276, -260,
	-244, -228, -212, -196, -180, -164, -148, -132,
	-120, -112, -104, -96, -88, -80, -72, -64,
	-56, -48, -40, -32, -24, -16, -8, 0,
	32124, 31100, 30076, 29052, 28028, 27004, 25980, 24956,
	23932, 22908, 21884, 20860, 19836, 18812, 17788, 16764,
	15996, 15484, 14972, 14460, 13948, 13436, 12924, 12412,
	11900, 11388, 10876, 10364, 9852, 9340, 8828, 8316,
	7932, 7676, 7420, 7164, 6908, 6652, 6396, 6140,
	5884, 5628, 5372, 5116, 4860, 4604, 4348, 4092,
	3900, 3772, 3644, 3516, 3388, 3260, 3132, 3004,
	2876, 2748, 2620, 2492, 2364, 2236, 2108, 1980,
	1884, 1820, 1756, 1692, 1628, 1564, 1500, 1436,
	1372, 1308, 1244, 1180, 1116, 1052, 988, 924,
	876, 844, 812, 780, 748, 716, 684, 652,
	620, 588, 556, 524, 492, 460, 428, 396,
	372, 356, 340, 324, 308, 292, 276, 260,
	244, 228, 212, 196, 180, 164, 148, 132,
	120, 112, 104, 96, 88, 80, 72, 64,
	56, 48, 40, 32, 24, 16, 8, 0,
}

// PCMU G.711 μ-law 编解码器。
type PCMU struct {
	frameSize int
}

// NewPCMU 创建 PCMU 编解码器实例。
func NewPCMU(frameSize int) *PCMU {
	if frameSize <= 0 {
		frameSize = 160 // 默认 20ms @ 8kHz
	}
	return &PCMU{frameSize: frameSize}
}

func (c *PCMU) Name() string          { return "PCMU" }
func (c *PCMU) PayloadType() int      { return 0 }
func (c *PCMU) ClockRate() int        { return 8000 }
func (c *PCMU) Channels() int         { return 1 }
func (c *PCMU) FrameSize() int        { return c.frameSize }
func (c *PCMU) SamplesPerPacket() int { return c.frameSize }

// Encode 将 16 位线性 PCM 编码为 μ-law。
func (c *PCMU) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, errors.New("codec: empty PCM data")
	}
	ulaw := make([]byte, len(pcm))
	for i, sample := range pcm {
		ulaw[i] = LinearToULaw(sample)
	}
	return ulaw, nil
}

// Decode 将 μ-law 解码为 16 位线性 PCM。
func (c *PCMU) Decode(data []byte) ([]int16, error) {
	if len(data) == 0 {
		return nil, errors.New("codec: empty data")
	}
	pcm := make([]int16, len(data))
	for i, b := range data {
		pcm[i] = ULawToLinear(b)
	}
	return pcm, nil
}

// LinearToULaw 将 16 位线性 PCM 样本转换为 μ-law。
// 实现 ITU-T G.711 标准算法。
func LinearToULaw(pcmVal int16) uint8 {
	const (
		MAX  = 0x7FFF
		BIAS = 0x84
		CLIP = 32635
	)

	// 获取符号位
	sign := uint8(0)
	if pcmVal < 0 {
		sign = 0x80
		pcmVal = -pcmVal
	}

	// 削波
	if pcmVal > CLIP {
		pcmVal = CLIP
	}

	// 添加偏置
	pcmVal = pcmVal + BIAS

	// 提取段和量化值
	exponent := int(ulawCompressTable[(pcmVal>>7)&0xFF])
	mantissa := (pcmVal >> (exponent + 3)) & 0x0F
	ulawByte := ^(sign | uint8(exponent<<4) | uint8(mantissa))

	// μ-law 需要取反所有位
	return ulawByte & 0xFF
}

// ULawToLinear 将 μ-law 样本转换为 16 位线性 PCM。
// 使用查找表实现，性能最优。
func ULawToLinear(ulaw uint8) int16 {
	return ulawDecodeTable[ulaw]
}

// ---- G.711 A-law (PCMA) 实现 ----

// A-law 参数
const (
	aLawSign    = 0x80
	aLawSegment = 0x70
	aLawQuantum = 0x0F
	aLawClip    = 32635
)

// alawCompressTable A-law 压缩查找表。
var alawCompressTable = [256]uint8{
	1, 1, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4,
	5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
}

// alawDecodeTable 已移除：使用算法实现替代，参见 ALawToLinear。

// PCMA G.711 A-law 编解码器。
type PCMA struct {
	frameSize int
}

// NewPCMA 创建 PCMA 编解码器实例。
func NewPCMA(frameSize int) *PCMA {
	if frameSize <= 0 {
		frameSize = 160
	}
	return &PCMA{frameSize: frameSize}
}

func (c *PCMA) Name() string          { return "PCMA" }
func (c *PCMA) PayloadType() int      { return 8 }
func (c *PCMA) ClockRate() int        { return 8000 }
func (c *PCMA) Channels() int         { return 1 }
func (c *PCMA) FrameSize() int        { return c.frameSize }
func (c *PCMA) SamplesPerPacket() int { return c.frameSize }

// Encode 将 16 位线性 PCM 编码为 A-law。
func (c *PCMA) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, errors.New("codec: empty PCM data")
	}
	alaw := make([]byte, len(pcm))
	for i, sample := range pcm {
		alaw[i] = LinearToALaw(sample)
	}
	return alaw, nil
}

// Decode 将 A-law 解码为 16 位线性 PCM。
func (c *PCMA) Decode(data []byte) ([]int16, error) {
	if len(data) == 0 {
		return nil, errors.New("codec: empty data")
	}
	pcm := make([]int16, len(data))
	for i, b := range data {
		pcm[i] = ALawToLinear(b)
	}
	return pcm, nil
}

// LinearToALaw 将 16 位线性 PCM 样本转换为 A-law。
// 实现 ITU-T G.711 标准算法（含 bias=33 偏置）。
func LinearToALaw(pcmVal int16) uint8 {
	const (
		BIAS = 33
		CLIP = 32635
	)

	// 获取符号位
	sign := uint8(0)
	if pcmVal < 0 {
		sign = 0x80
		pcmVal = -pcmVal
	}

	// 削波
	if pcmVal > CLIP {
		pcmVal = CLIP
	}

	// 添加偏置（G.711 标准要求）
	pcmVal += BIAS

	// 计算段和量化值
	var compressed uint8
	if pcmVal >= 512 {
		// 使用查找表获取段号（基于偏置后的值）
		exponent := int(alawCompressTable[(pcmVal>>8)&0xFF])
		mantissa := (pcmVal >> (exponent + 3)) & 0x0F
		compressed = sign | uint8(exponent<<4) | uint8(mantissa)
	} else {
		// 小信号：线性段
		compressed = sign | uint8(pcmVal>>4)
	}

	// A-law 需要交替位翻转
	return compressed ^ 0x55
}

// ALawToLinear 将 A-law 样本转换为 16 位线性 PCM。
// 实现 ITU-T G.711 标准算法。
func ALawToLinear(alaw uint8) int16 {
	// 先反转交替位
	alaw ^= 0x55

	// 提取符号位
	sign := alaw & 0x80
	alaw &= 0x7F

	// 提取段号和量化值
	segment := (alaw >> 4) & 0x07
	mantissa := alaw & 0x0F

	// 根据段号重建线性值
	// 公式: value = ((16*mantissa + 8) * 2^(segment-1)) + (2^segment - 1) * 32
	// 等价位运算实现
	var pcm int16
	switch segment {
	case 0:
		pcm = int16(mantissa)<<4 + 8
	case 1:
		pcm = int16(mantissa)<<5 + 40
	default:
		pcm = (int16(mantissa)+16)<<(segment+3) - 24
	}

	if sign != 0 {
		return -pcm
	}
	return pcm
}

// ---- 编解码器注册表 ----

// Registry 编解码器注册表。
type Registry struct {
	codecs map[string]func() Codec
	mu     sync.RWMutex
}

// DefaultRegistry 默认编解码器注册表。
var DefaultRegistry = NewRegistry()

// NewRegistry 创建编解码器注册表。
func NewRegistry() *Registry {
	r := &Registry{
		codecs: make(map[string]func() Codec),
	}
	// 注册标准编解码器
	r.Register("PCMU", func() Codec { return NewPCMU(160) })
	r.Register("PCMA", func() Codec { return NewPCMA(160) })
	return r
}

// Register 注册编解码器工厂。
func (r *Registry) Register(name string, factory func() Codec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codecs[name] = factory
}

// Get 获取编解码器实例。
func (r *Registry) Get(name string) (Codec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.codecs[name]
	if !ok {
		return nil, errors.New("codec: unknown codec: " + name)
	}
	return factory(), nil
}

// List 列出所有已注册的编解码器名称。
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.codecs))
	for name := range r.codecs {
		names = append(names, name)
	}
	return names
}
