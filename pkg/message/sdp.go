package message

import (
	"fmt"
	"strconv"
	"strings"
)

// SDP 表示一个完整的 SDP 会话描述（RFC 4566）。
type SDP struct {
	Version     int            // v=
	Origin      SDPOrigin      // o=
	SessionName string         // s=
	Info        string         // i=
	URI         string         // u=
	Email       string         // e=
	Phone       string         // p=
	Connection  *SDPConnection // c= (session-level)
	Bandwidth   []SDPBandwidth // b= (session-level)
	TimeZones   []SDPTimeZone  // z=
	Keys        []SDPKey       // k= (session-level)
	Attributes  []SDPAttribute // a= (session-level)
	Media       []SDPMedia     // m= sections
}

// SDPOrigin 表示 SDP o= 行。
type SDPOrigin struct {
	Username       string
	SessionID      uint64
	SessionVersion uint64
	NetType        string // "IN"
	AddrType       string // "IP4" 或 "IP6"
	Address        string
}

// String 返回 o= 行的字符串表示。
func (o *SDPOrigin) String() string {
	return fmt.Sprintf("%s %d %d %s %s %s",
		o.Username, o.SessionID, o.SessionVersion,
		o.NetType, o.AddrType, o.Address)
}

// SDPConnection 表示 SDP c= 行。
type SDPConnection struct {
	NetType  string // "IN"
	AddrType string // "IP4" 或 "IP6"
	Address  string
	TTL      int // 仅用于 IPv4 组播
	NumAddrs int // 仅用于组播
}

// String 返回 c= 行的字符串表示。
func (c *SDPConnection) String() string {
	s := fmt.Sprintf("%s %s %s", c.NetType, c.AddrType, c.Address)
	if c.TTL > 0 {
		s = fmt.Sprintf("%s %s %s/%d", c.NetType, c.AddrType, c.Address, c.TTL)
	}
	if c.NumAddrs > 0 {
		s = fmt.Sprintf("%s/%d", s, c.NumAddrs)
	}
	return s
}

// SDPBandwidth 表示 SDP b= 行。
type SDPBandwidth struct {
	Type  string // "CT", "AS", "TIAS" 等
	Value int    // kbps
}

// String 返回 b= 行的字符串表示。
func (b *SDPBandwidth) String() string {
	return fmt.Sprintf("%s:%d", b.Type, b.Value)
}

// SDPTimeZone 表示 SDP z= 行。
type SDPTimeZone struct {
	Time   string
	Offset string
}

// SDPKey 表示 SDP k= 行。
type SDPKey struct {
	Type  string // "prompt", "clear", "base64", "uri"
	Value string
}

// SDPAttribute 表示 SDP a= 行。
type SDPAttribute struct {
	Name  string
	Value string
}

// String 返回 a= 行的字符串表示。
func (a *SDPAttribute) String() string {
	if a.Value != "" {
		return a.Name + ":" + a.Value
	}
	return a.Name
}

// SDPMedia 表示一个 SDP m= 媒体描述段。
type SDPMedia struct {
	Type       string // "audio", "video", "application" 等
	Port       int
	NumPorts   int            // 端口数（可选）
	Protocol   string         // "RTP/AVP", "RTP/SAVP", "RTP/SAVPF" 等
	Formats    []string       // payload type 列表
	Connection *SDPConnection // c= (media-level, 覆盖 session-level)
	Bandwidth  []SDPBandwidth // b= (media-level)
	Keys       []SDPKey       // k= (media-level)
	Attributes []SDPAttribute // a= (media-level)
}

// GetAttribute 获取媒体段中指定名称的属性值。
func (m *SDPMedia) GetAttribute(name string) (string, bool) {
	for _, a := range m.Attributes {
		if a.Name == name {
			return a.Value, true
		}
	}
	return "", false
}

// GetRtpmap 解析 rtpmap 属性，返回 codec name 和 clock rate。
func (m *SDPMedia) GetRtpmap(pt string) (string, int, bool) {
	for _, a := range m.Attributes {
		if a.Name == "rtpmap" && strings.HasPrefix(a.Value, pt+" ") {
			parts := strings.SplitN(a.Value, " ", 3)
			if len(parts) >= 2 {
				codecParts := strings.Split(parts[1], "/")
				if len(codecParts) >= 2 {
					rate, _ := strconv.Atoi(codecParts[1])
					return codecParts[0], rate, true
				}
			}
		}
	}
	return "", 0, false
}

// String 返回 m= 行的字符串表示。
func (m *SDPMedia) String() string {
	var buf strings.Builder
	buf.WriteString("m=")
	buf.WriteString(m.Type)
	buf.WriteByte(' ')
	buf.WriteString(strconv.Itoa(m.Port))
	if m.NumPorts > 0 {
		buf.WriteByte('/')
		buf.WriteString(strconv.Itoa(m.NumPorts))
	}
	buf.WriteByte(' ')
	buf.WriteString(m.Protocol)
	for _, f := range m.Formats {
		buf.WriteByte(' ')
		buf.WriteString(f)
	}
	buf.WriteString("\r\n")

	if m.Connection != nil {
		buf.WriteString("c=")
		buf.WriteString(m.Connection.String())
		buf.WriteString("\r\n")
	}
	for _, b := range m.Bandwidth {
		buf.WriteString("b=")
		buf.WriteString(b.String())
		buf.WriteString("\r\n")
	}
	for _, a := range m.Attributes {
		buf.WriteString("a=")
		buf.WriteString(a.String())
		buf.WriteString("\r\n")
	}
	return buf.String()
}

// ParseSDP 解析 SDP 文本为 SDP 结构体。
func ParseSDP(data []byte) (*SDP, error) {
	sdp := &SDP{}
	lines := strings.Split(string(data), "\r\n")
	if len(lines) == 0 {
		lines = strings.Split(string(data), "\n")
	}

	var currentMedia *SDPMedia

	for _, line := range lines {
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		lineType := line[0]
		value := line[2:]

		switch lineType {
		case 'v':
			v, _ := strconv.Atoi(value)
			sdp.Version = v
		case 'o':
			sdp.Origin = parseOrigin(value)
		case 's':
			sdp.SessionName = value
		case 'i':
			if currentMedia != nil {
				// media-level info, 忽略
			} else {
				sdp.Info = value
			}
		case 'u':
			sdp.URI = value
		case 'e':
			sdp.Email = value
		case 'p':
			sdp.Phone = value
		case 'c':
			conn := parseConnection(value)
			if currentMedia != nil {
				currentMedia.Connection = conn
			} else {
				sdp.Connection = conn
			}
		case 'b':
			bw := parseBandwidth(value)
			if currentMedia != nil {
				currentMedia.Bandwidth = append(currentMedia.Bandwidth, bw)
			} else {
				sdp.Bandwidth = append(sdp.Bandwidth, bw)
			}
		case 'z':
			// 简化处理
		case 'k':
			key := parseKey(value)
			if currentMedia != nil {
				currentMedia.Keys = append(currentMedia.Keys, key)
			} else {
				sdp.Keys = append(sdp.Keys, key)
			}
		case 't':
			// time description, 简化处理
		case 'm':
			// 新的媒体段
			if currentMedia != nil {
				sdp.Media = append(sdp.Media, *currentMedia)
			}
			currentMedia = parseMedia(value)
		case 'a':
			attr := parseAttribute(value)
			if currentMedia != nil {
				currentMedia.Attributes = append(currentMedia.Attributes, attr)
			} else {
				sdp.Attributes = append(sdp.Attributes, attr)
			}
		}
	}

	if currentMedia != nil {
		sdp.Media = append(sdp.Media, *currentMedia)
	}

	return sdp, nil
}

func parseOrigin(s string) SDPOrigin {
	parts := strings.Fields(s)
	o := SDPOrigin{}
	if len(parts) >= 6 {
		o.Username = parts[0]
		o.SessionID, _ = strconv.ParseUint(parts[1], 10, 64)
		o.SessionVersion, _ = strconv.ParseUint(parts[2], 10, 64)
		o.NetType = parts[3]
		o.AddrType = parts[4]
		o.Address = parts[5]
	}
	return o
}

func parseConnection(s string) *SDPConnection {
	parts := strings.Fields(s)
	c := &SDPConnection{}
	if len(parts) >= 3 {
		c.NetType = parts[0]
		c.AddrType = parts[1]
		c.Address = parts[2]
	}
	return c
}

func parseBandwidth(s string) SDPBandwidth {
	parts := strings.SplitN(s, ":", 2)
	bw := SDPBandwidth{}
	if len(parts) >= 2 {
		bw.Type = parts[0]
		bw.Value, _ = strconv.Atoi(parts[1])
	}
	return bw
}

func parseKey(s string) SDPKey {
	parts := strings.SplitN(s, ":", 2)
	k := SDPKey{}
	if len(parts) >= 2 {
		k.Type = parts[0]
		k.Value = parts[1]
	} else {
		k.Type = s
	}
	return k
}

func parseMedia(s string) *SDPMedia {
	parts := strings.Fields(s)
	m := &SDPMedia{}
	if len(parts) >= 3 {
		m.Type = parts[0]
		// 解析端口（可能包含 /numPorts）
		portStr := parts[1]
		if slashIdx := strings.IndexByte(portStr, '/'); slashIdx >= 0 {
			m.Port, _ = strconv.Atoi(portStr[:slashIdx])
			m.NumPorts, _ = strconv.Atoi(portStr[slashIdx+1:])
		} else {
			m.Port, _ = strconv.Atoi(portStr)
		}
		m.Protocol = parts[2]
		m.Formats = parts[3:]
	}
	return m
}

func parseAttribute(s string) SDPAttribute {
	colonIdx := strings.IndexByte(s, ':')
	if colonIdx >= 0 {
		return SDPAttribute{Name: s[:colonIdx], Value: s[colonIdx+1:]}
	}
	return SDPAttribute{Name: s}
}

// BuildSDP 将 SDP 结构体序列化为文本。
func BuildSDP(sdp *SDP) []byte {
	var buf strings.Builder

	buf.WriteString("v=")
	buf.WriteString(strconv.Itoa(sdp.Version))
	buf.WriteString("\r\n")

	buf.WriteString("o=")
	buf.WriteString(sdp.Origin.String())
	buf.WriteString("\r\n")

	buf.WriteString("s=")
	buf.WriteString(sdp.SessionName)
	buf.WriteString("\r\n")

	if sdp.Info != "" {
		buf.WriteString("i=")
		buf.WriteString(sdp.Info)
		buf.WriteString("\r\n")
	}

	if sdp.Connection != nil {
		buf.WriteString("c=")
		buf.WriteString(sdp.Connection.String())
		buf.WriteString("\r\n")
	}

	for _, bw := range sdp.Bandwidth {
		buf.WriteString("b=")
		buf.WriteString(bw.String())
		buf.WriteString("\r\n")
	}

	buf.WriteString("t=0 0\r\n")

	for _, attr := range sdp.Attributes {
		buf.WriteString("a=")
		buf.WriteString(attr.String())
		buf.WriteString("\r\n")
	}

	for _, media := range sdp.Media {
		buf.WriteString(media.String())
	}

	return []byte(buf.String())
}
