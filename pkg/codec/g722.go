package codec

import (
	"errors"
)

// G.722 参数常量
const (
	G722SampleRate = 16000
	G722FrameSize  = 320 // 20ms @ 16kHz = 320 samples
	G722MaxBitrate = 64000
)

// QMF 滤波器系数（ITU-T G.722 标准）
var qmfCoeffs = [24]int32{
	3, -11, -11, 53, 12, -156, -32, 362,
	-84, -815, 315, 2339, -1776, -6331, 10087, 30853,
	10087, -6331, -1776, 2339, 315, -815, -84, 362,
}

// ADPCM 量化步长表（ITU-T G.722 标准）
var q6 = [64]int16{
	0, 35, 71, 108, 146, 185, 225, 265,
	306, 349, 392, 437, 483, 531, 580, 631,
	683, 737, 793, 851, 911, 973, 1038, 1105,
	1175, 1248, 1324, 1404, 1488, 1576, 1669, 1767,
	1870, 1979, 2094, 2216, 2345, 2482, 2627, 2781,
	2944, 3118, 3303, 3501, 3713, 3941, 4188, 4456,
	4749, 5069, 5420, 5807, 6236, 6713, 7247, 7850,
	8540, 9344, 10296, 11452, 12904, 14811, 17584, 22304,
}

var q2 = [4]int16{0, 1, 0, 1}

// 逆量化步长因子
var ilb = [32]int16{
	2048, 2093, 2139, 2186, 2233, 2282, 2332, 2383,
	2435, 2489, 2543, 2599, 2656, 2714, 2774, 2834,
	2896, 2960, 3025, 3091, 3158, 3228, 3298, 3371,
	3444, 3520, 3597, 3676, 3756, 3838, 3922, 4008,
}

// 自适应速度控制参数
var wl = [8]int16{-60, -30, 58, 131, 70, -5, -65, -115}
var wb = [4]int16{-214, 70, 178, 218}

// G722 G.722 宽编解码器。
type G722 struct {
	frameSize int
	mode      G722Mode
	// 编码器状态
	encStateLow  adpcmState
	encStateHigh adpcmState
	encQmfX      [24]int32
	// 解码器状态
	decStateLow  adpcmState
	decStateHigh adpcmState
	decQmfX      [24]int32
}

// G722Mode G.722 工作模式（不同比特率）。
type G722Mode int

const (
	G722Mode64k G722Mode = 64 // 64kbps
	G722Mode56k G722Mode = 56 // 56kbps
	G722Mode48k G722Mode = 48 // 48kbps
)

// adpcmState ADPCM 子带编码器状态。
type adpcmState struct {
	s   int32 // 信号预测值
	sp  int32 // 预测值
	sz  int32 // 零极点预测值
	r   int32 // 重建信号
	nb  int32 // 步长指数
	dl  int32 // 量化距离
	ap  int32 // 部分重建信号
	ton int32 // 音调检测
}

// NewG722 创建 G.722 编解码器实例。
func NewG722(mode G722Mode) *G722 {
	if mode != G722Mode64k && mode != G722Mode56k && mode != G722Mode48k {
		mode = G722Mode64k
	}
	return &G722{
		frameSize: G722FrameSize,
		mode:      mode,
	}
}

func (c *G722) Name() string          { return "G722" }
func (c *G722) PayloadType() int      { return 9 }
func (c *G722) ClockRate() int        { return 8000 } // RTP 时钟率仍为 8kHz
func (c *G722) Channels() int         { return 1 }
func (c *G722) FrameSize() int        { return c.frameSize }
func (c *G722) SamplesPerPacket() int { return c.frameSize }

// Encode 将 16kHz PCM 编码为 G.722。
func (c *G722) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, errors.New("codec: empty PCM data")
	}
	if len(pcm)%2 != 0 {
		return nil, errors.New("codec: PCM length must be even for G.722")
	}

	// 计算输出大小
	var bitsPerSample int
	switch c.mode {
	case G722Mode64k:
		bitsPerSample = 8
	case G722Mode56k:
		bitsPerSample = 7
	case G722Mode48k:
		bitsPerSample = 6
	}

	numSamples := len(pcm) / 2 // 每两个输入样本产生一个编码样本
	outputBits := numSamples * bitsPerSample
	outputBytes := (outputBits + 7) / 8
	output := make([]byte, outputBytes)

	bitPos := 0
	for i := 0; i < len(pcm); i += 2 {
		// QMF 分析：将 16kHz 信号分为低频和高频子带
		xout, xout2 := c.qmfAnalysis(int32(pcm[i]), int32(pcm[i+1]))

		// 低频子带 ADPCM 编码（6 bits for 64k mode）
		var lowBits int
		switch c.mode {
		case G722Mode64k:
			lowBits = c.adpcmEncodeLow(xout, 6)
		case G722Mode56k:
			lowBits = c.adpcmEncodeLow(xout, 5)
		case G722Mode48k:
			lowBits = c.adpcmEncodeLow(xout, 4)
		}

		// 高频子带 ADPCM 编码（2 bits）
		var highBits int
		if c.mode == G722Mode64k {
			highBits = c.adpcmEncodeHigh(xout2, 2)
		} else if c.mode == G722Mode56k {
			highBits = c.adpcmEncodeHigh(xout2, 1)
		}
		// 48k mode: 不编码高频

		// 打包位
		switch c.mode {
		case G722Mode64k:
			output[bitPos/8] = byte((lowBits << 2) | highBits)
			bitPos += 8
		case G722Mode56k:
			// 7 bits: 5 low + 2 high
			packBits(output, bitPos, (lowBits<<2)|highBits, 7)
			bitPos += 7
		case G722Mode48k:
			// 6 bits: 4 low + 2 high
			packBits(output, bitPos, (lowBits<<2)|highBits, 6)
			bitPos += 6
		}
	}

	return output, nil
}

// Decode 将 G.722 解码为 16kHz PCM。
func (c *G722) Decode(data []byte) ([]int16, error) {
	if len(data) == 0 {
		return nil, errors.New("codec: empty data")
	}

	// 计算样本数
	var bitsPerSample int
	switch c.mode {
	case G722Mode64k:
		bitsPerSample = 8
	case G722Mode56k:
		bitsPerSample = 7
	case G722Mode48k:
		bitsPerSample = 6
	}

	totalBits := len(data) * 8
	numSamples := totalBits / bitsPerSample
	pcm := make([]int16, numSamples*2)

	bitPos := 0
	for i := 0; i < numSamples; i++ {
		var lowCode, highCode int

		switch c.mode {
		case G722Mode64k:
			b := data[bitPos/8]
			lowCode = int(b >> 2)
			highCode = int(b & 0x03)
			bitPos += 8
		case G722Mode56k:
			val := unpackBits(data, bitPos, 7)
			lowCode = int(val >> 2)
			highCode = int(val & 0x03)
			bitPos += 7
		case G722Mode48k:
			val := unpackBits(data, bitPos, 6)
			lowCode = int(val >> 2)
			highCode = int(val & 0x03)
			bitPos += 6
		}

		// 低频子带 ADPCM 解码
		var lowBits int
		switch c.mode {
		case G722Mode64k:
			lowBits = 6
		case G722Mode56k:
			lowBits = 5
		case G722Mode48k:
			lowBits = 4
		}
		xlow := c.adpcmDecodeLow(lowCode, lowBits)

		// 高频子带 ADPCM 解码
		var xhigh int32
		if c.mode == G722Mode64k {
			xhigh = c.adpcmDecodeHigh(highCode, 2)
		} else if c.mode == G722Mode56k {
			xhigh = c.adpcmDecodeHigh(highCode, 1)
		}

		// QMF 合成：将子带信号合并为 16kHz 信号
		s0, s1 := c.qmfSynthesis(xlow, xhigh)
		pcm[i*2] = int16(clamp16(s0))
		pcm[i*2+1] = int16(clamp16(s1))
	}

	return pcm, nil
}

// qmfAnalysis QMF 分析滤波器：将 16kHz 输入分为两个 8kHz 子带。
func (c *G722) qmfAnalysis(x0, x1 int32) (int32, int32) {
	// 移位输入缓冲区
	for i := 23; i > 1; i-- {
		c.encQmfX[i] = c.encQmfX[i-2]
	}
	c.encQmfX[0] = x0
	c.encQmfX[1] = x1

	// 应用 QMF 滤波器
	var sumLow, sumHigh int32
	for i := 0; i < 24; i += 2 {
		sumLow += qmfCoeffs[i] * c.encQmfX[i]
	}
	for i := 1; i < 24; i += 2 {
		sumHigh += qmfCoeffs[i] * c.encQmfX[i]
	}

	// 输出低频和高频子带样本
	return sumLow >> 14, sumHigh >> 14
}

// qmfSynthesis QMF 合成滤波器：将两个子带合并为 16kHz 输出。
func (c *G722) qmfSynthesis(xlow, xhigh int32) (int32, int32) {
	// 移位输入缓冲区
	for i := 23; i > 1; i-- {
		c.decQmfX[i] = c.decQmfX[i-2]
	}
	c.decQmfX[0] = xlow + xhigh
	c.decQmfX[1] = xlow - xhigh

	// 应用 QMF 滤波器
	var s0, s1 int32
	for i := 0; i < 24; i++ {
		s0 += qmfCoeffs[i] * c.decQmfX[i]
	}
	for i := 0; i < 24; i++ {
		if i%2 == 0 {
			s1 += qmfCoeffs[i] * c.decQmfX[i]
		} else {
			s1 -= qmfCoeffs[i] * c.decQmfX[i]
		}
	}

	return s0 >> 14, s1 >> 14
}

// adpcmEncodeLow 低频子带 ADPCM 编码。
func (c *G722) adpcmEncodeLow(x int32, bits int) int {
	d := x - (c.encStateLow.s + c.encStateLow.sz)

	// 量化
	var il int32
	var sign int32
	if d >= 0 {
		sign = 0
	} else {
		sign = 1
		d = -d
	}

	// 使用步长表量化
	dlt := int32(q6[c.encStateLow.nb])
	if dlt == 0 {
		dlt = 1
	}
	il = (d << 2) / dlt

	// 限制量化范围
	maxIL := int32((1 << uint(bits-1)) - 1)
	if il > maxIL {
		il = maxIL
	}

	// 组合符号和量化值
	if sign != 0 {
		il = (1 << uint(bits-1)) | il
	}

	// 更新 ADPCM 状态
	c.adpcmUpdateLow(il, bits)

	return int(il)
}

// adpcmEncodeHigh 高频子带 ADPCM 编码。
func (c *G722) adpcmEncodeHigh(x int32, bits int) int {
	d := x - (c.encStateHigh.s + c.encStateHigh.sz)

	var ih int32
	var sign int32
	if d >= 0 {
		sign = 0
	} else {
		sign = 1
		d = -d
	}

	// 高频固定步长量化
	dlt := int32(q2[c.encStateHigh.nb&0x03])
	if dlt == 0 {
		dlt = 1
	}
	ih = (d << 2) / dlt

	maxIH := int32((1 << uint(bits-1)) - 1)
	if ih > maxIH {
		ih = maxIH
	}

	if sign != 0 {
		ih = (1 << uint(bits-1)) | ih
	}

	c.adpcmUpdateHigh(ih, bits)

	return int(ih)
}

// adpcmDecodeLow 低频子带 ADPCM 解码。
func (c *G722) adpcmDecodeLow(code int, bits int) int32 {
	// 更新状态
	c.adpcmUpdateLow(int32(code), bits)

	// 返回重建信号
	return c.decStateLow.s + c.decStateLow.sz
}

// adpcmDecodeHigh 高频子带 ADPCM 解码。
func (c *G722) adpcmDecodeHigh(code int, bits int) int32 {
	c.adpcmUpdateHigh(int32(code), bits)
	return c.decStateHigh.s + c.decStateHigh.sz
}

// adpcmUpdateLow 更新低频 ADPCM 编码器状态。
func (c *G722) adpcmUpdateLow(il int32, bits int) {
	// 逆量化
	sign := (il >> uint(bits-1)) & 1
	mag := il & ((1 << uint(bits-1)) - 1)

	dlt := int32(q6[c.encStateLow.nb])
	if sign != 0 {
		c.encStateLow.r = c.encStateLow.s + c.encStateLow.sz - dlt*int32(mag+1)
	} else {
		c.encStateLow.r = c.encStateLow.s + c.encStateLow.sz + dlt*int32(mag+1)
	}

	// 限制范围
	c.encStateLow.r = clamp32(c.encStateLow.r, -16384, 16383)

	// 更新步长指数
	c.encStateLow.nb = int32(wl[mag&0x07]) + int32((c.encStateLow.nb*127)>>7)
	if c.encStateLow.nb < 0 {
		c.encStateLow.nb = 0
	}
	if c.encStateLow.nb > 63 {
		c.encStateLow.nb = 63
	}

	// 更新预测器
	c.encStateLow.s = c.encStateLow.r
	c.encStateLow.sp = c.encStateLow.s
}

// adpcmUpdateHigh 更新高频 ADPCM 编码器状态。
func (c *G722) adpcmUpdateHigh(ih int32, bits int) {
	sign := (ih >> uint(bits-1)) & 1
	mag := ih & ((1 << uint(bits-1)) - 1)

	dlt := int32(q2[c.encStateHigh.nb&0x03])
	if sign != 0 {
		c.encStateHigh.r = c.encStateHigh.s + c.encStateHigh.sz - dlt*int32(mag+1)
	} else {
		c.encStateHigh.r = c.encStateHigh.s + c.encStateHigh.sz + dlt*int32(mag+1)
	}

	c.encStateHigh.r = clamp32(c.encStateHigh.r, -16384, 16383)

	// 高频步长更新简化
	c.encStateHigh.nb = int32(wb[mag&0x03]) + int32((c.encStateHigh.nb*127)>>7)
	if c.encStateHigh.nb < 0 {
		c.encStateHigh.nb = 0
	}
	if c.encStateHigh.nb > 3 {
		c.encStateHigh.nb = 3
	}

	c.encStateHigh.s = c.encStateHigh.r
	c.encStateHigh.sp = c.encStateHigh.s
}

// packBits 将值打包到位流中。
func packBits(data []byte, bitPos int, value int, numBits int) {
	for i := 0; i < numBits; i++ {
		byteIdx := (bitPos + i) / 8
		bitIdx := 7 - ((bitPos + i) % 8)
		if byteIdx < len(data) {
			if (value>>uint(numBits-1-i))&1 != 0 {
				data[byteIdx] |= 1 << uint(bitIdx)
			}
		}
	}
}

// unpackBits 从位流中解包值。
func unpackBits(data []byte, bitPos int, numBits int) int {
	var value int
	for i := 0; i < numBits; i++ {
		byteIdx := (bitPos + i) / 8
		bitIdx := 7 - ((bitPos + i) % 8)
		if byteIdx < len(data) {
			if (data[byteIdx]>>uint(bitIdx))&1 != 0 {
				value |= 1 << uint(numBits-1-i)
			}
		}
	}
	return value
}

// clamp16 限制值在 16 位范围内。
func clamp16(v int32) int32 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return v
}

// clamp32 限制值在指定范围内。
func clamp32(v, min, max int32) int32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func init() {
	// 注册 G.722 到默认注册表
	DefaultRegistry.Register("G722", func() Codec { return NewG722(G722Mode64k) })
}
