// tlsfingerprint.go 出站 TLS ClientHello 指纹复刻（Node 22 / OpenSSL 3.x）。
package upstream

import (
	"crypto/tls"
	"fmt"
	"os"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// TLS 指纹档位。
const (
	// TLSFingerprintNode 复刻官方 Node/OpenSSL 形态（生产默认）。
	TLSFingerprintNode = "node"
	// TLSFingerprintGo 用 crypto/tls 原生形态（排障/回滚用）。
	TLSFingerprintGo = "go"
)

// 真机实测常量（用于回归断言）。
const (
	OfficialJA3MD5 = "71dc8c533dd919ae9f4963224a4ba8fd"
	OfficialJA3    = "771," +
		"4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53," +
		"0-23-65281-10-11-35-13-51-45-43," +
		"29-23-24," +
		"0"
	OfficialJA4 = "t13d0a1200_5d04281c6031_b76c5ee2507d"
)

var officialCipherSuites = []uint16{
	tls.TLS_AES_128_GCM_SHA256,                        // 0x1301 4865
	tls.TLS_AES_256_GCM_SHA384,                        // 0x1302 4866
	tls.TLS_CHACHA20_POLY1305_SHA256,                  // 0x1303 4867
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,         // 0xc02f 49199
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,       // 0xc02b 49195
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,         // 0xc030 49200
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,       // 0xc02c 49196
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,         // 0xc027 49191
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, // 0xcca9 52393
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,   // 0xcca8 52392
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,          // 0xc009 49161
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,            // 0xc013 49171
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,          // 0xc00a 49162
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,            // 0xc014 49172
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,               // 0x009c 156
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,               // 0x009d 157
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,                  // 0x002f 47
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,                  // 0x0035 53
}

var officialSigAlgs = []utls.SignatureScheme{
	0x0403, // ecdsa_secp256r1_sha256
	0x0804, // rsa_pss_rsae_sha256
	0x0401, // rsa_pkcs1_sha256
	0x0503, // ecdsa_secp384r1_sha384
	0x0805, // rsa_pss_rsae_sha384
	0x0501, // rsa_pkcs1_sha384
	0x0806, // rsa_pss_rsae_sha512
	0x0601, // rsa_pkcs1_sha512
	0x0201, // rsa_pkcs1_sha1
}

// officialHelloSpec 复刻官方 Node/OpenSSL ClientHello（无 ALPN，匹配 Node 主进程形态）。
func officialHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
		CipherSuites:       officialCipherSuites,
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},                      // 0
			&utls.UtlsExtendedMasterSecretExtension{}, // 23
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient}, // 65281
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519,    // 29
				utls.CurveP256, // 23
				utls.CurveP384, // 24
			}}, // 10
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}}, // 11
			&utls.SessionTicketExtension{},                             // 35
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: officialSigAlgs}, // 13
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519},
			}}, // 51
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}}, // 45
			&utls.SupportedVersionsExtension{Versions: []uint16{
				tls.VersionTLS13,
				tls.VersionTLS12,
			}}, // 43
		},
	}
}

// useNodeFingerprint 决定是否走 uTLS Node 指纹；WB2A_TLS_FINGERPRINT=go 环境变量最高优先回退。
func useNodeFingerprint(fp string) bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("WB2A_TLS_FINGERPRINT")), TLSFingerprintGo) {
		return false
	}
	return strings.ToLower(strings.TrimSpace(fp)) != TLSFingerprintGo
}

// ValidateTLSFingerprint 校验并归一化配置取值，空值回落 "node"。
func ValidateTLSFingerprint(fp string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(fp)); v {
	case "", TLSFingerprintNode:
		return TLSFingerprintNode, nil
	case TLSFingerprintGo:
		return TLSFingerprintGo, nil
	default:
		return "", fmt.Errorf("upstream.tls_fingerprint 取值 %q 无效（可选 node / go）", fp)
	}
}

func validateTLSFingerprint(fp string) (string, error) { return ValidateTLSFingerprint(fp) }
