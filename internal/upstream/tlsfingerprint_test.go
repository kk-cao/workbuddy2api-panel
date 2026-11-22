package upstream

import (
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
	"time"
)

type helloFields struct {
	version           uint16
	ciphers           []uint16
	exts              []uint16
	groups            []uint16
	ecpf              []uint8
	sigalgs           []uint16
	supportedVersions []uint16
	alpn              []string
	hasSNI            bool
	sessionIDLen      int
	recordLen         int
}

func parseClientHello(t *testing.T, raw []byte) *helloFields {
	t.Helper()
	f := &helloFields{recordLen: len(raw)}
	p := 5
	if len(raw) < p+4 || raw[p] != 1 {
		t.Fatalf("不是 ClientHello（首字节 %d）", raw[0])
	}
	p += 4
	f.version = binary.BigEndian.Uint16(raw[p : p+2])
	p += 2 + 32
	sidLen := int(raw[p])
	f.sessionIDLen = sidLen
	p += 1 + sidLen
	csLen := int(binary.BigEndian.Uint16(raw[p : p+2]))
	p += 2
	for i := 0; i+1 < csLen; i += 2 {
		f.ciphers = append(f.ciphers, binary.BigEndian.Uint16(raw[p+i:p+i+2]))
	}
	p += csLen
	cmLen := int(raw[p])
	p += 1 + cmLen

	if p+2 > len(raw) {
		t.Fatal("ClientHello 截断（无扩展段）")
	}
	total := int(binary.BigEndian.Uint16(raw[p : p+2]))
	p += 2
	end := p + total
	if end > len(raw) {
		end = len(raw)
	}
	for p+4 <= end {
		et := binary.BigEndian.Uint16(raw[p : p+2])
		el := int(binary.BigEndian.Uint16(raw[p+2 : p+4]))
		data := raw[p+4:]
		if len(data) > el {
			data = data[:el]
		}
		f.exts = append(f.exts, et)
		switch et {
		case 0:
			f.hasSNI = true
		case 10:
			for i := 2; i+1 < len(data); i += 2 {
				f.groups = append(f.groups, binary.BigEndian.Uint16(data[i:i+2]))
			}
		case 11:
			for i := 1; i < len(data); i++ {
				f.ecpf = append(f.ecpf, data[i])
			}
		case 13:
			if len(data) >= 2 {
				n := int(binary.BigEndian.Uint16(data[0:2]))
				for i := 2; i+1 < len(data) && i < 2+n; i += 2 {
					f.sigalgs = append(f.sigalgs, binary.BigEndian.Uint16(data[i:i+2]))
				}
			}
		case 43:
			if len(data) >= 1 {
				n := int(data[0])
				for i := 1; i+1 < len(data) && i < 1+n; i += 2 {
					f.supportedVersions = append(f.supportedVersions, binary.BigEndian.Uint16(data[i:i+2]))
				}
			}
		case 16:
			if len(data) >= 2 {
				q := 2
				for q < len(data) {
					l := int(data[q])
					if q+1+l > len(data) {
						break
					}
					f.alpn = append(f.alpn, string(data[q+1:q+1+l]))
					q += 1 + l
				}
			}
		}
		p += 4 + el
	}
	return f
}

func ja4Version(v uint16) string {
	return fmt.Sprintf("1%d", int(v&0xff)-1)
}

func (f *helloFields) maxSupportedVersion() uint16 {
	var out uint16
	for _, v := range f.supportedVersions {
		if v > out {
			out = v
		}
	}
	return out
}

func (f *helloFields) ja3() string {
	join := func(list []string) string { return strings.Join(list, "-") }
	var cs, es, gs, ef []string
	for _, c := range f.ciphers {
		cs = append(cs, fmt.Sprintf("%d", c))
	}
	for _, e := range f.exts {
		es = append(es, fmt.Sprintf("%d", e))
	}
	for _, g := range f.groups {
		gs = append(gs, fmt.Sprintf("%d", g))
	}
	for _, x := range f.ecpf {
		ef = append(ef, fmt.Sprintf("%d", x))
	}
	return fmt.Sprintf("%d,%s,%s,%s,%s", f.version, join(cs), join(es), join(gs), join(ef))
}

func (f *helloFields) ja4() string {
	tlsMax := f.version
	if v := f.maxSupportedVersion(); v > tlsMax {
		tlsMax = v
	}
	a := fmt.Sprintf("t%s%s%02x%02x", ja4Version(tlsMax), map[bool]string{true: "d", false: "i"}[f.hasSNI],
		len(f.exts), len(f.ciphers))
	if len(f.alpn) > 0 && len(f.alpn[0]) >= 2 {
		a += f.alpn[0][:2]
	} else {
		a += "00"
	}
	var cs []int
	for _, c := range f.ciphers {
		cs = append(cs, int(c))
	}
	sort.Ints(cs)
	var bIn []string
	for _, c := range cs {
		bIn = append(bIn, fmt.Sprintf("%04x", c))
	}
	b := sha256.Sum256([]byte(strings.Join(bIn, ",")))

	var ex, sa []int
	for _, e := range f.exts {
		if e != 0 && e != 16 {
			ex = append(ex, int(e))
		}
	}
	for _, s := range f.sigalgs {
		sa = append(sa, int(s))
	}
	sort.Ints(ex)
	sort.Ints(sa)
	var cIn, saIn []string
	for _, e := range ex {
		cIn = append(cIn, fmt.Sprintf("%04x", e))
	}
	for _, s := range sa {
		saIn = append(saIn, fmt.Sprintf("%04x", s))
	}
	c := sha256.Sum256([]byte(strings.Join(cIn, ",") + "_" + strings.Join(saIn, ",")))
	return fmt.Sprintf("%s_%s_%s", a, hex.EncodeToString(b[:])[:12], hex.EncodeToString(c[:])[:12])
}

func captureClientHello(t *testing.T, dial func(addr string) (net.Conn, error)) []byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	got := make(chan []byte, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			got <- nil
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 8192)
		n, _ := c.Read(buf)
		got <- buf[:n]
	}()

	go func() {
		if conn, derr := dial(ln.Addr().String()); derr == nil {
			conn.Close()
		}
	}()

	select {
	case b := <-got:
		if len(b) == 0 {
			t.Fatal("没抓到 ClientHello")
		}
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("等 ClientHello 超时")
		return nil
	}
}

func dialFor(t *testing.T, fp string) func(addr string) (net.Conn, error) {
	t.Helper()
	d := newDialer()
	cfg := &tls.Config{InsecureSkipVerify: true, ServerName: "www.workbuddy.cn"}
	tlsDial := newTLSDialer(d, cfg, fp)
	return func(addr string) (net.Conn, error) {
		conn, _ := tlsDial(t.Context(), "tcp", addr)
		if conn == nil {
			return nil, net.ErrClosed
		}
		return conn, nil
	}
}

func TestNodeFingerprintMatchesOfficial(t *testing.T) {
	raw := captureClientHello(t, dialFor(t, TLSFingerprintNode))
	f := parseClientHello(t, raw)

	if got := f.ja3(); got != OfficialJA3 {
		t.Errorf("JA3 与官方不一致\n  want: %s\n  got : %s", OfficialJA3, got)
	}
	if md5Hex(f.ja3()) != OfficialJA3MD5 {
		t.Errorf("JA3 MD5 = %s want %s", md5Hex(f.ja3()), OfficialJA3MD5)
	}
	if got := f.ja4(); got != OfficialJA4 {
		t.Errorf("JA4 与官方不一致\n  want: %s\n  got : %s", OfficialJA4, got)
	}
	if len(f.ciphers) != 18 {
		t.Errorf("cipher 数 = %d want 18", len(f.ciphers))
	}
	if len(f.exts) != 10 {
		t.Errorf("扩展数 = %d want 10", len(f.exts))
	}
	if len(f.sigalgs) != 9 {
		t.Errorf("sigalgs = %d want 9: %v", len(f.sigalgs), f.sigalgs)
	}
	if len(f.alpn) != 0 {
		t.Errorf("ALPN 必须为空，got %v", f.alpn)
	}
	if got := f.groups; fmt.Sprint(got) != "[29 23 24]" {
		t.Errorf("groups = %v want [29 23 24]", got)
	}
	if f.sessionIDLen != 32 {
		t.Errorf("session_id 长度 = %d want 32", f.sessionIDLen)
	}
}

func TestGoFingerprintDiffersFromOfficial(t *testing.T) {
	raw := captureClientHello(t, dialFor(t, TLSFingerprintGo))
	f := parseClientHello(t, raw)
	if f.ja3() == OfficialJA3 {
		t.Fatal("go 档位不该产出官方指纹——开关失效")
	}
	if len(f.alpn) != 0 {
		t.Errorf("go 档位也应无 ALPN，got %v", f.alpn)
	}
}

func TestEnvOverridesNodeToGo(t *testing.T) {
	t.Setenv("WB2A_TLS_FINGERPRINT", "go")
	raw := captureClientHello(t, dialFor(t, TLSFingerprintNode))
	f := parseClientHello(t, raw)
	if f.ja3() == OfficialJA3 {
		t.Fatal("WB2A_TLS_FINGERPRINT=go 应回退原生握手")
	}
}

func TestTLSFingerprintValidation(t *testing.T) {
	cases := map[string]string{
		"":       TLSFingerprintNode,
		"node":   TLSFingerprintNode,
		"NODE":   TLSFingerprintNode,
		" go ":   TLSFingerprintGo,
		"go":     TLSFingerprintGo,
	}
	for in, want := range cases {
		got, err := validateTLSFingerprint(in)
		if err != nil {
			t.Errorf("validate(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("validate(%q) = %q want %q", in, got, want)
		}
	}
	if _, err := validateTLSFingerprint("chrome"); err == nil {
		t.Error("非法取值应报错")
	}
}

func TestSetTLSFingerprintSwitches(t *testing.T) {
	c := New()
	defer c.HTTP.CloseIdleConnections()
	defer c.ChatHTTP.CloseIdleConnections()

	if c.TLSFingerprint != TLSFingerprintNode {
		t.Fatalf("默认档位 = %q want node", c.TLSFingerprint)
	}
	if err := c.SetTLSFingerprint(TLSFingerprintGo); err != nil {
		t.Fatalf("切到 go: %v", err)
	}
	if c.TLSFingerprint != TLSFingerprintGo {
		t.Fatalf("切换后档位 = %q", c.TLSFingerprint)
	}
	if err := c.SetTLSFingerprint("bogus"); err == nil {
		t.Error("非法档位应报错")
	}
	if c.TLSFingerprint != TLSFingerprintGo {
		t.Errorf("非法切换后档位被污染 = %q", c.TLSFingerprint)
	}
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

var _ = tls.VersionTLS13
