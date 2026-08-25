package media

import (
	"testing"
	"time"
)

// ---- DTMF 事件转换测试 ----

func TestDigitToEvent(t *testing.T) {
	tests := []struct {
		digit rune
		event DTMFEvent
		err   bool
	}{
		{'0', DTMFEvent0, false},
		{'1', DTMFEvent1, false},
		{'2', DTMFEvent2, false},
		{'3', DTMFEvent3, false},
		{'4', DTMFEvent4, false},
		{'5', DTMFEvent5, false},
		{'6', DTMFEvent6, false},
		{'7', DTMFEvent7, false},
		{'8', DTMFEvent8, false},
		{'9', DTMFEvent9, false},
		{'*', DTMFEventStar, false},
		{'#', DTMFEventHash, false},
		{'a', DTMFEventA, false},
		{'b', DTMFEventB, false},
		{'c', DTMFEventC, false},
		{'d', DTMFEventD, false},
		{'X', 0, true},
		{' ', 0, true},
	}

	for _, tt := range tests {
		event, err := DigitToEvent(tt.digit)
		if tt.err {
			if err == nil {
				t.Errorf("DigitToEvent(%c): expected error, got nil", tt.digit)
			}
			continue
		}
		if err != nil {
			t.Errorf("DigitToEvent(%c): unexpected error: %v", tt.digit, err)
			continue
		}
		if event != tt.event {
			t.Errorf("DigitToEvent(%c): got %d, want %d", tt.digit, event, tt.event)
		}
	}
}

func TestEventToDigit(t *testing.T) {
	tests := []struct {
		event DTMFEvent
		digit rune
		err   bool
	}{
		{DTMFEvent0, '0', false},
		{DTMFEvent5, '5', false},
		{DTMFEvent9, '9', false},
		{DTMFEventStar, '*', false},
		{DTMFEventHash, '#', false},
		{DTMFEventA, 'A', false},
		{DTMFEventD, 'D', false},
		{DTMFEvent(99), 0, true},
	}

	for _, tt := range tests {
		digit, err := EventToDigit(tt.event)
		if tt.err {
			if err == nil {
				t.Errorf("EventToDigit(%d): expected error, got nil", tt.event)
			}
			continue
		}
		if err != nil {
			t.Errorf("EventToDigit(%d): unexpected error: %v", tt.event, err)
			continue
		}
		if digit != tt.digit {
			t.Errorf("EventToDigit(%d): got %c, want %c", tt.event, digit, tt.digit)
		}
	}
}

func TestDTMFEventString(t *testing.T) {
	tests := []struct {
		event DTMFEvent
		str   string
	}{
		{DTMFEvent0, "0"},
		{DTMFEvent9, "9"},
		{DTMFEventStar, "*"},
		{DTMFEventHash, "#"},
		{DTMFEventA, "A"},
		{DTMFEventD, "D"},
		{DTMFEventFlash, "Flash"},
	}

	for _, tt := range tests {
		if got := tt.event.String(); got != tt.str {
			t.Errorf("DTMFEvent(%d).String(): got %q, want %q", tt.event, got, tt.str)
		}
	}
}

// ---- DTMF Payload 编解码测试 ----

func TestDTMFPayloadEncodeDecode(t *testing.T) {
	tests := []struct {
		name    string
		payload DTMFPayload
	}{
		{
			name: "digit 5, end=false",
			payload: DTMFPayload{
				Event:    DTMFEvent5,
				End:      false,
				Volume:   0,
				Duration: 160,
			},
		},
		{
			name: "digit star, end=true",
			payload: DTMFPayload{
				Event:    DTMFEventStar,
				End:      true,
				Volume:   10,
				Duration: 800,
			},
		},
		{
			name: "digit hash, end=true, max volume",
			payload: DTMFPayload{
				Event:    DTMFEventHash,
				End:      true,
				Volume:   63,
				Duration: 65535,
			},
		},
		{
			name: "digit 0, end=false, zero duration",
			payload: DTMFPayload{
				Event:    DTMFEvent0,
				End:      false,
				Volume:   0,
				Duration: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := tt.payload.Encode()
			if err != nil {
				t.Fatalf("Encode(): unexpected error: %v", err)
			}
			if len(encoded) != 4 {
				t.Fatalf("Encode(): got %d bytes, want 4", len(encoded))
			}

			decoded, err := DecodeDTMFPayload(encoded)
			if err != nil {
				t.Fatalf("DecodeDTMFPayload(): unexpected error: %v", err)
			}

			if decoded.Event != tt.payload.Event {
				t.Errorf("Event: got %d, want %d", decoded.Event, tt.payload.Event)
			}
			if decoded.End != tt.payload.End {
				t.Errorf("End: got %v, want %v", decoded.End, tt.payload.End)
			}
			if decoded.Volume != tt.payload.Volume {
				t.Errorf("Volume: got %d, want %d", decoded.Volume, tt.payload.Volume)
			}
			if decoded.Duration != tt.payload.Duration {
				t.Errorf("Duration: got %d, want %d", decoded.Duration, tt.payload.Duration)
			}
		})
	}
}

func TestDecodeDTMFPayloadTooShort(t *testing.T) {
	_, err := DecodeDTMFPayload([]byte{0x05, 0x80})
	if err == nil {
		t.Error("DecodeDTMFPayload(): expected error for short payload, got nil")
	}
}

func TestDTMFPayloadBitLayout(t *testing.T) {
	// 验证 RTP payload 位布局
	p := DTMFPayload{
		Event:    DTMFEvent3,
		End:      true,
		Volume:   0x15,   // 21
		Duration: 0x0100, // 256
	}

	encoded, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}

	// Byte 0: event
	if encoded[0] != byte(DTMFEvent3) {
		t.Errorf("byte 0 (event): got 0x%02x, want 0x%02x", encoded[0], DTMFEvent3)
	}

	// Byte 1: E(1) | R(1) | volume(6)
	// E=1 -> 0x80, volume=0x15 -> 0x80|0x15 = 0x95
	expected1 := byte(0x80 | 0x15)
	if encoded[1] != expected1 {
		t.Errorf("byte 1 (E|R|vol): got 0x%02x, want 0x%02x", encoded[1], expected1)
	}

	// Byte 2-3: duration (big endian)
	if encoded[2] != 0x01 || encoded[3] != 0x00 {
		t.Errorf("byte 2-3 (duration): got 0x%02x%02x, want 0x0100", encoded[2], encoded[3])
	}
}

// ---- DTMF Sender 测试 ----

func TestDTMFSenderBuildPackets(t *testing.T) {
	sender := NewDTMFSender(0x12345678, 8000)

	packets := sender.BuildDTMFEventPackets(DTMFEvent5, 200, 1000)

	if len(packets) == 0 {
		t.Fatal("BuildDTMFEventPackets(): no packets generated")
	}

	// 验证至少有 1 个初始包 + 冗余包 + 3 个结束包
	if len(packets) < 5 {
		t.Errorf("BuildDTMFEventPackets(): expected at least 5 packets, got %d", len(packets))
	}

	// 验证第一个包（初始，End=0）
	firstPkt := packets[0]
	if len(firstPkt.Data) != 16 {
		t.Errorf("first packet size: got %d, want 16", len(firstPkt.Data))
	}

	// 验证 RTP 头
	if firstPkt.Data[0] != 0x80 {
		t.Errorf("first packet V/P/X/CC: got 0x%02x, want 0x80", firstPkt.Data[0])
	}

	// 验证 SSRC
	ssrc := uint32(firstPkt.Data[8])<<24 | uint32(firstPkt.Data[9])<<16 |
		uint32(firstPkt.Data[10])<<8 | uint32(firstPkt.Data[11])
	if ssrc != 0x12345678 {
		t.Errorf("SSRC: got 0x%08x, want 0x12345678", ssrc)
	}

	// 验证最后一个包（结束，End=1）
	lastPkt := packets[len(packets)-1]
	dtmfPayload, err := DecodeDTMFPayload(lastPkt.Data[12:])
	if err != nil {
		t.Fatal(err)
	}
	if !dtmfPayload.End {
		t.Error("last packet: End bit should be true")
	}
	if dtmfPayload.Event != DTMFEvent5 {
		t.Errorf("last packet event: got %d, want %d", dtmfPayload.Event, DTMFEvent5)
	}
}

// ---- DTMF Receiver 测试 ----

func TestDTMFReceiverProcessEvent(t *testing.T) {
	receiver := NewDTMFReceiver()

	var receivedEvent DTMFEvent
	var receivedDuration time.Duration
	eventReceived := false

	receiver.OnDTMFEvent(func(event DTMFEvent, duration time.Duration) {
		receivedEvent = event
		receivedDuration = duration
		eventReceived = true
	})

	// 发送开始包（End=0）
	startPayload := DTMFPayload{
		Event:    DTMFEvent7,
		End:      false,
		Volume:   0,
		Duration: 160,
	}
	startData, _ := startPayload.Encode()

	_, _, ok := receiver.ProcessRTPPacket(startData, 1000)
	if ok {
		t.Error("start packet should not complete event")
	}

	// 发送结束包（End=1）
	endPayload := DTMFPayload{
		Event:    DTMFEvent7,
		End:      true,
		Volume:   0,
		Duration: 160,
	}
	endData, _ := endPayload.Encode()

	event, duration, ok := receiver.ProcessRTPPacket(endData, 1000)
	if !ok {
		t.Error("end packet should complete event")
	}
	if event != DTMFEvent7 {
		t.Errorf("event: got %d, want %d", event, DTMFEvent7)
	}
	if duration == 0 {
		t.Error("duration should be > 0")
	}

	if !eventReceived {
		t.Error("callback should have been called")
	}
	if receivedEvent != DTMFEvent7 {
		t.Errorf("callback event: got %d, want %d", receivedEvent, DTMFEvent7)
	}
	if receivedDuration == 0 {
		t.Error("callback duration should be > 0")
	}

	if receiver.EventsReceived() != 1 {
		t.Errorf("EventsReceived: got %d, want 1", receiver.EventsReceived())
	}
}

func TestDTMFReceiverInvalidPayload(t *testing.T) {
	receiver := NewDTMFReceiver()

	// 太短的 payload
	_, _, ok := receiver.ProcessRTPPacket([]byte{0x01, 0x02}, 1000)
	if ok {
		t.Error("short payload should not be processed")
	}
}

// ---- DTMF SDP 辅助测试 ----

func TestBuildDTMFSDPAttr(t *testing.T) {
	attr := BuildDTMFSDPAttr(101, 8000)
	expected := "a=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-16"
	if attr != expected {
		t.Errorf("BuildDTMFSDPAttr: got %q, want %q", attr, expected)
	}
}

func TestParseDTMFSDPAttr(t *testing.T) {
	sdp := "v=0\r\n" +
		"m=audio 49170 RTP/AVP 0 101\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\n" +
		"a=fmtp:101 0-16\r\n"

	pt, found := ParseDTMFSDPAttr(sdp)
	if !found {
		t.Fatal("ParseDTMFSDPAttr: telephone-event not found")
	}
	if pt != 101 {
		t.Errorf("ParseDTMFSDPAttr: got PT %d, want 101", pt)
	}
}

func TestParseDTMFSDPAttrNotFound(t *testing.T) {
	sdp := "v=0\r\nm=audio 49170 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
	_, found := ParseDTMFSDPAttr(sdp)
	if found {
		t.Error("ParseDTMFSDPAttr: should not find telephone-event")
	}
}

// ---- Digit/Event 往返测试 ----

func TestDigitEventRoundTrip(t *testing.T) {
	digits := []rune{'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '*', '#'}

	for _, digit := range digits {
		event, err := DigitToEvent(digit)
		if err != nil {
			t.Errorf("DigitToEvent(%c): %v", digit, err)
			continue
		}

		result, err := EventToDigit(event)
		if err != nil {
			t.Errorf("EventToDigit(%d): %v", event, err)
			continue
		}

		if result != digit {
			t.Errorf("roundtrip %c -> %d -> %c", digit, event, result)
		}
	}
}
