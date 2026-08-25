package codec

import (
	"testing"
)

// ---- PCMU 测试 ----

func TestPCMUEncodeDecode(t *testing.T) {
	codec := NewPCMU(160)

	// 生成测试 PCM 数据
	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = int16(i * 100)
	}

	// 编码
	encoded, err := codec.Encode(pcm)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(encoded) != 160 {
		t.Fatalf("encoded length: got %d, want 160", len(encoded))
	}

	// 解码
	decoded, err := codec.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(decoded) != 160 {
		t.Fatalf("decoded length: got %d, want 160", len(decoded))
	}

	// G.711 是有损压缩，验证误差在可接受范围内
	for i := range pcm {
		diff := int(pcm[i]) - int(decoded[i])
		if diff < 0 {
			diff = -diff
		}
		// 允许最大 5% 的误差（G.711 量化噪声）
		maxErr := int(int16Abs(pcm[i])/20) + 16
		if maxErr < 32 {
			maxErr = 32
		}
		if diff > maxErr {
			t.Errorf("sample %d: original=%d, decoded=%d, diff=%d (max allowed=%d)",
				i, pcm[i], decoded[i], diff, maxErr)
		}
	}
}

func TestPCMUProperties(t *testing.T) {
	codec := NewPCMU(160)
	if codec.Name() != "PCMU" {
		t.Errorf("Name: got %s, want PCMU", codec.Name())
	}
	if codec.PayloadType() != 0 {
		t.Errorf("PayloadType: got %d, want 0", codec.PayloadType())
	}
	if codec.ClockRate() != 8000 {
		t.Errorf("ClockRate: got %d, want 8000", codec.ClockRate())
	}
	if codec.Channels() != 1 {
		t.Errorf("Channels: got %d, want 1", codec.Channels())
	}
}

func TestPCMUEmptyInput(t *testing.T) {
	codec := NewPCMU(160)
	_, err := codec.Encode(nil)
	if err == nil {
		t.Error("Encode(nil): expected error, got nil")
	}
	_, err = codec.Decode(nil)
	if err == nil {
		t.Error("Decode(nil): expected error, got nil")
	}
}

// ---- PCMA 测试 ----

func TestPCMAEncodeDecode(t *testing.T) {
	codec := NewPCMA(160)

	pcm := make([]int16, 160)
	for i := range pcm {
		pcm[i] = int16(i * 100)
	}

	encoded, err := codec.Encode(pcm)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(encoded) != 160 {
		t.Fatalf("encoded length: got %d, want 160", len(encoded))
	}

	decoded, err := codec.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(decoded) != 160 {
		t.Fatalf("decoded length: got %d, want 160", len(decoded))
	}

	// 验证误差
	for i := range pcm {
		diff := int(pcm[i]) - int(decoded[i])
		if diff < 0 {
			diff = -diff
		}
		// G.711 A-law 对数压缩误差（含 bias=33 偏移和段边界量化误差）
		maxErr := int(int16Abs(pcm[i])/2) + 64
		if diff > maxErr {
			t.Errorf("sample %d: original=%d, decoded=%d, diff=%d", i, pcm[i], decoded[i], diff)
		}
	}
}

func TestPCMAProperties(t *testing.T) {
	codec := NewPCMA(160)
	if codec.Name() != "PCMA" {
		t.Errorf("Name: got %s, want PCMA", codec.Name())
	}
	if codec.PayloadType() != 8 {
		t.Errorf("PayloadType: got %d, want 8", codec.PayloadType())
	}
	if codec.ClockRate() != 8000 {
		t.Errorf("ClockRate: got %d, want 8000", codec.ClockRate())
	}
}

// ---- G.722 测试 ----

func TestG722EncodeDecode(t *testing.T) {
	codec := NewG722(G722Mode64k)

	// G.722 需要 16kHz 采样，20ms = 320 样本
	pcm := make([]int16, 320)
	for i := range pcm {
		pcm[i] = int16(i * 50)
	}

	encoded, err := codec.Encode(pcm)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("Encode: produced empty output")
	}

	decoded, err := codec.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(decoded) == 0 {
		t.Fatal("Decode: produced empty output")
	}
}

func TestG722Modes(t *testing.T) {
	modes := []G722Mode{G722Mode64k, G722Mode56k, G722Mode48k}

	for _, mode := range modes {
		codec := NewG722(mode)
		if codec.Name() != "G722" {
			t.Errorf("mode %d: Name: got %s, want G722", mode, codec.Name())
		}
	}
}

func TestG722EmptyInput(t *testing.T) {
	codec := NewG722(G722Mode64k)
	_, err := codec.Encode(nil)
	if err == nil {
		t.Error("Encode(nil): expected error")
	}
	_, err = codec.Decode(nil)
	if err == nil {
		t.Error("Decode(nil): expected error")
	}
}

// ---- 编解码器注册表测试 ----

func TestRegistryGet(t *testing.T) {
	codec, err := DefaultRegistry.Get("PCMU")
	if err != nil {
		t.Fatalf("Get(PCMU): %v", err)
	}
	if codec.Name() != "PCMU" {
		t.Errorf("Name: got %s, want PCMU", codec.Name())
	}
}

func TestRegistryGetUnknown(t *testing.T) {
	_, err := DefaultRegistry.Get("UnknownCodec")
	if err == nil {
		t.Error("Get(UnknownCodec): expected error, got nil")
	}
}

func TestRegistryList(t *testing.T) {
	list := DefaultRegistry.List()
	if len(list) < 2 {
		t.Errorf("List: expected at least 2 codecs, got %d", len(list))
	}

	found := map[string]bool{}
	for _, name := range list {
		found[name] = true
	}

	for _, expected := range []string{"PCMU", "PCMA", "G722"} {
		if !found[expected] {
			t.Errorf("List: missing codec %s", expected)
		}
	}
}

func TestRegistryCustomCodec(t *testing.T) {
	r := NewRegistry()
	r.Register("test", func() Codec { return NewPCMU(80) })

	codec, err := r.Get("test")
	if err != nil {
		t.Fatalf("Get(test): %v", err)
	}
	if codec.FrameSize() != 80 {
		t.Errorf("FrameSize: got %d, want 80", codec.FrameSize())
	}
}

// ---- LinearToULaw/ULawToLinear 往返测试 ----

func TestULawRoundTrip(t *testing.T) {
	testValues := []int16{0, 100, 1000, 5000, 16000, 32000, -100, -1000, -16000, -32000}

	for _, val := range testValues {
		encoded := LinearToULaw(val)
		decoded := ULawToLinear(encoded)

		diff := int(val) - int(decoded)
		if diff < 0 {
			diff = -diff
		}
		// G.711 允许一定误差
		maxErr := int(int16Abs(val)/16) + 32
		if diff > maxErr {
			t.Errorf("ULaw roundtrip: %d -> 0x%02x -> %d (diff=%d, max=%d)",
				val, encoded, decoded, diff, maxErr)
		}
	}
}

// ---- LinearToALaw/ALawToLinear 往返测试 ----

func TestALawRoundTrip(t *testing.T) {
	testValues := []int16{0, 100, 1000, 5000, 16000, 32000, -100, -1000, -16000, -32000}

	for _, val := range testValues {
		encoded := LinearToALaw(val)
		decoded := ALawToLinear(encoded)

		diff := int(val) - int(decoded)
		if diff < 0 {
			diff = -diff
		}
		maxErr := int(int16Abs(val)/16) + 48
		if diff > maxErr {
			t.Errorf("ALaw roundtrip: %d -> 0x%02x -> %d (diff=%d, max=%d)",
				val, encoded, decoded, diff, maxErr)
		}
	}
}

// ---- 辅助函数 ----

func int16Abs(v int16) int16 {
	if v < 0 {
		return -v
	}
	return v
}
