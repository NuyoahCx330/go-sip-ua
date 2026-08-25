package media

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NuyoahCx330/go-sip-ua/pkg/logger"
)

// TranscodeSession 转码会话接口。
type TranscodeSession interface {
	// Start 启动转码。
	Start(ctx context.Context) error
	// Stop 停止转码。
	Stop() error
	// SetInput 设置输入端（接收媒体）。
	SetInput(addr *net.UDPAddr, codec *Codec) error
	// SetOutput 设置输出端（发送媒体）。
	SetOutput(addr *net.UDPAddr, codec *Codec) error
	// Stats 获取转码统计。
	Stats() *TranscodeStats
	// Pause 暂停/恢复转码。
	Pause(pause bool)
}

// TranscodeStats 转码统计。
type TranscodeStats struct {
	PacketsDecoded   atomic.Int64
	PacketsEncoded   atomic.Int64
	PacketsForwarded atomic.Int64
	PacketsDropped   atomic.Int64
	BytesProcessed   atomic.Int64
	StartTime        time.Time
}

// transcodeSession 是 TranscodeSession 的默认实现。
type transcodeSession struct {
	config   TranscodeConfig
	log      logger.Logger
	input    *net.UDPConn
	output   *net.UDPAddr
	inCodec  *Codec
	outCodec *Codec
	inAudio  AudioCodec // 实际音频编解码器实例
	outAudio AudioCodec // 实际音频编解码器实例
	pipeline *TranscodePipeline
	stats    TranscodeStats
	paused   atomic.Bool
	doneCh   chan struct{}
	wg       sync.WaitGroup
	mu       sync.RWMutex
	started  bool
}

// NewTranscodeSession 创建转码会话。
func NewTranscodeSession(cfg TranscodeConfig, log logger.Logger) TranscodeSession {
	if log == nil {
		log = logger.NopLogger()
	}
	return &transcodeSession{
		config: cfg,
		log:    log,
		doneCh: make(chan struct{}),
	}
}

func (t *transcodeSession) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return errors.New("media: transcode session already started")
	}
	t.started = true
	t.stats.StartTime = time.Now()
	t.mu.Unlock()

	t.wg.Add(1)
	go t.transcodeLoop(ctx)

	t.log.Info("media", "transcode session started")
	return nil
}

func (t *transcodeSession) Stop() error {
	t.mu.Lock()
	if !t.started {
		t.mu.Unlock()
		return nil
	}
	t.started = false
	t.mu.Unlock()

	close(t.doneCh)
	if t.input != nil {
		t.input.Close()
	}
	t.wg.Wait()
	return nil
}

func (t *transcodeSession) SetInput(addr *net.UDPAddr, codec *Codec) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.input != nil {
		t.input.Close()
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("media: transcode listen input %s: %w", addr, err)
	}
	t.input = conn
	t.inCodec = codec
	t.inAudio = resolveAudioCodec(codec)
	t.rebuildPipeline()
	t.log.Info("media", "transcode: input set to %s (codec: %s)", addr, codec)
	return nil
}

func (t *transcodeSession) SetOutput(addr *net.UDPAddr, codec *Codec) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.output = addr
	t.outCodec = codec
	t.outAudio = resolveAudioCodec(codec)
	t.rebuildPipeline()
	t.log.Info("media", "transcode: output set to %s (codec: %s)", addr, codec)
	return nil
}

// rebuildPipeline 根据输入/输出编解码器重建转码管道。
func (t *transcodeSession) rebuildPipeline() {
	if t.inAudio != nil && t.outAudio != nil {
		t.pipeline = NewTranscodePipeline(t.inAudio, t.outAudio)
	}
}

// resolveAudioCodec 根据 Codec 描述查找实际的音频编解码器实例。
func resolveAudioCodec(c *Codec) AudioCodec {
	if c == nil {
		return nil
	}
	switch c.Name {
	case "PCMU", "G711u":
		return NewPCMUCodec()
	case "PCMA", "G711a":
		return NewPCMACodec()
	case "G722":
		return NewG722Codec()
	default:
		// 尝试查找外部编解码器
		if ext, err := FindExternalCodec(c.Name); err == nil {
			return ext
		}
		return nil
	}
}

func (t *transcodeSession) Stats() *TranscodeStats {
	return &t.stats
}

func (t *transcodeSession) Pause(pause bool) {
	t.paused.Store(pause)
}

func (t *transcodeSession) transcodeLoop(ctx context.Context) {
	defer t.wg.Done()
	buf := make([]byte, 65535)

	for {
		select {
		case <-t.doneCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		if t.paused.Load() {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		t.mu.RLock()
		input := t.input
		output := t.output
		inCodec := t.inCodec
		outCodec := t.outCodec
		t.mu.RUnlock()

		if input == nil || output == nil || inCodec == nil || outCodec == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		input.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := input.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-t.doneCh:
				return
			default:
				t.log.Debug("media", "transcode: read error: %v", err)
				continue
			}
		}

		if n < 12 {
			t.stats.PacketsDropped.Add(1)
			continue
		}

		t.stats.PacketsDecoded.Add(1)
		t.stats.BytesProcessed.Add(int64(n))

		// 解析 RTP 包
		_, _, seq, ts, ssrc, payload := parseRTPPacketFull(buf[:n])

		var outPayload []byte
		t.mu.RLock()
		pipeline := t.pipeline
		outCodecInst := t.outAudio
		t.mu.RUnlock()

		if pipeline != nil && outCodecInst != nil {
			// 实际转码：解码 → PCM → 编码
			transcoded, err := pipeline.Transcode(payload)
			if err != nil {
				t.stats.PacketsDropped.Add(1)
				t.log.Debug("media", "transcode: transcode error: %v", err)
				continue
			}
			outPayload = transcoded
		} else if outCodecInst != nil {
			// 仅解码
			pcm, err := outCodecInst.Decode(payload)
			if err != nil {
				t.stats.PacketsDropped.Add(1)
				continue
			}
			_ = pcm
			outPayload = payload // 回退到直接转发
		} else {
			// 无编解码器，直接转发
			outPayload = payload
		}

		// 重新构建输出包（使用输出编解码器的 PT）
		outPT := 0
		if t.outCodec != nil {
			outPT = t.outCodec.PayloadType
		}
		outPacket := buildRTPPacket(outPT, seq, ts, ssrc, outPayload)

		_, err = input.WriteToUDP(outPacket, output)
		if err != nil {
			t.stats.PacketsDropped.Add(1)
			t.log.Debug("media", "transcode: write error: %v", err)
			continue
		}

		t.stats.PacketsEncoded.Add(1)
		t.stats.PacketsForwarded.Add(1)
	}
}

// parseRTPPacketFull 完整解析 RTP 包。
func parseRTPPacketFull(data []byte) (version int, pt int, seq uint16, ts uint32, ssrc uint32, payload []byte) {
	if len(data) < 12 {
		return
	}
	version = int((data[0] >> 6) & 0x03)
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

// MediaTranscoder 转码管理器。
type MediaTranscoder interface {
	// CreateSession 创建转码会话。
	CreateSession(cfg TranscodeConfig) (TranscodeSession, error)
	// StopAll 停止所有转码会话。
	StopAll() error
}

// mediaTranscoder 是 MediaTranscoder 的默认实现。
type mediaTranscoder struct {
	sessions sync.Map
	log      logger.Logger
}

// NewMediaTranscoder 创建转码管理器。
func NewMediaTranscoder(log logger.Logger) MediaTranscoder {
	if log == nil {
		log = logger.NopLogger()
	}
	return &mediaTranscoder{log: log}
}

func (mt *mediaTranscoder) CreateSession(cfg TranscodeConfig) (TranscodeSession, error) {
	session := NewTranscodeSession(cfg, mt.log)
	id := fmt.Sprintf("transcode-%d", time.Now().UnixNano())
	mt.sessions.Store(id, session)
	return session, nil
}

func (mt *mediaTranscoder) StopAll() error {
	mt.sessions.Range(func(key, value interface{}) bool {
		if session, ok := value.(TranscodeSession); ok {
			session.Stop()
		}
		mt.sessions.Delete(key)
		return true
	})
	return nil
}
