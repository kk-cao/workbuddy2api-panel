// transport.go 出站 Transport 构造与 TLS 指纹/头序连接包装的单一事实来源。
package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

const (
	dialTimeout           = 10 * time.Second
	dialKeepAlive         = 15 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	idleConnTimeout       = 30 * time.Second
	responseHeaderTimeout = 60 * time.Second

	maxIdleConns        = 100
	maxIdleConnsPerHost = 20
)

func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
}

func newTLSDialer(d *net.Dialer, cfg *tls.Config, fp string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return tlsHandshake(ctx, raw, addr, cfg, fp)
	}
}

func tlsHandshake(ctx context.Context, raw net.Conn, addr string, cfg *tls.Config, fp string) (net.Conn, error) {
	insecure := cfg != nil && cfg.InsecureSkipVerify
	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = addr
	}
	if cfg != nil && cfg.ServerName != "" {
		host = cfg.ServerName
	}
	hctx, cancel := context.WithTimeout(ctx, tlsHandshakeTimeout)
	defer cancel()

	if useNodeFingerprint(fp) {
		uc := utls.UClient(raw, &utls.Config{
			ServerName:         host,
			InsecureSkipVerify: insecure,
			MinVersion:         tls.VersionTLS12,
		}, utls.HelloCustom)
		if perr := uc.ApplyPreset(officialHelloSpec()); perr != nil {
			raw.Close()
			return nil, fmt.Errorf("tls fingerprint preset: %w", perr)
		}
		if herr := uc.HandshakeContext(hctx); herr != nil {
			raw.Close()
			return nil, herr
		}
		return newHeadOrderConn(uc), nil
	}

	var tc0 *tls.Config
	if cfg != nil {
		tc0 = cfg.Clone()
	} else {
		tc0 = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tc0.ServerName = host
	tc0.NextProtos = nil
	tc := tls.Client(raw, tc0)
	if herr := tc.HandshakeContext(hctx); herr != nil {
		raw.Close()
		return nil, herr
	}
	return newHeadOrderConn(tc), nil
}

// rebuildTLSDialer 用当前 dialer 与 TLS 指纹档位刷新 HTTP 与 ChatHTTP 的拨号器。
func (c *Client) rebuildTLSDialer() {
	d := c.dialer
	if d == nil {
		d = newDialer()
	}
	for _, cl := range []*http.Client{c.HTTP, c.ChatHTTP} {
		if cl == nil {
			continue
		}
		if t, ok := cl.Transport.(*http.Transport); ok {
			t.DialContext = d.DialContext
			if t.DialTLSContext != nil {
				t.DialTLSContext = newTLSDialer(d, nil, c.TLSFingerprint)
			}
		}
	}
}

// SetTLSFingerprint 切换出站 ClientHello 指纹档位（"node" / "go"）。
func (c *Client) SetTLSFingerprint(fp string) error {
	norm, err := validateTLSFingerprint(fp)
	if err != nil {
		return err
	}
	c.TLSFingerprint = norm
	c.rebuildTLSDialer()
	return nil
}

func newTransport() *http.Transport {
	return newTransportWithDialer(newDialer())
}

func newTransportWithDialer(dialer *net.Dialer) *http.Transport {
	if dialer == nil {
		dialer = newDialer()
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		DialTLSContext:        newTLSDialer(dialer, nil, TLSFingerprintNode),
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		Proxy:                 http.ProxyFromEnvironment,
	}
}

type closeIdler interface {
	CloseIdleConnections()
}

func roundTripCloseIdle(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}
