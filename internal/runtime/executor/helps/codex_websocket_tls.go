package helps

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"slices"

	tls "github.com/refraction-networking/utls"
)

// CodexWebsocketTLSHandshake matches the initial rustls ClientHello observed in
// Codex CLI 0.162.1 and 0.163.0. This is not a replacement for the rustls record
// layer. No session cache is shared between accounts and no ALPN is offered.
func CodexWebsocketTLSHandshake(ctx context.Context, raw net.Conn, serverName string) (net.Conn, error) {
	return codexWebsocketTLSHandshake(ctx, raw, &tls.Config{ServerName: serverName})
}

func codexWebsocketTLSHandshake(ctx context.Context, raw net.Conn, cfg *tls.Config) (net.Conn, error) {
	conn := tls.UClient(raw, codexFreshTLSConfig(cfg), tls.HelloCustom)
	if err := conn.ApplyPreset(codexWebsocketHello()); err != nil {
		return nil, fmt.Errorf("codex websocket TLS profile: %w", err)
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("codex websocket TLS handshake: %w", err)
	}
	return conn, nil
}

func codexWebsocketHello() *tls.ClientHelloSpec {
	var seed [2]byte
	rand.Read(seed[:])
	return codexWebsocketHelloWithOrderSeed(binary.BigEndian.Uint16(seed[:]))
}

func codexWebsocketHelloWithOrderSeed(seed uint16) *tls.ClientHelloSpec {
	type extension struct {
		id    uint16
		value tls.TLSExtension
	}
	ordered := []extension{
		{0, &tls.SNIExtension{}},
		{5, &tls.StatusRequestExtension{}},
		{10, &tls.SupportedCurvesExtension{Curves: []tls.CurveID{4588, 29, 23, 24}}},
		{11, &tls.SupportedPointsExtension{SupportedPoints: []byte{0}}},
		{13, &tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{1283, 1027, 1539, 2055, 2054, 2053, 2052, 1537, 1281, 1025, 2308, 2309, 2310}}},
		{23, &tls.ExtendedMasterSecretExtension{}},
		{35, &tls.SessionTicketExtension{}},
		{43, &tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}}},
		{45, &tls.PSKKeyExchangeModesExtension{Modes: []uint8{1}}},
		{51, &tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: 4588}, {Group: 29}}}},
	}
	// Codex 0.163.0 pins rustls 0.23.45: ordinary extensions are sorted by
	// a hash of a fresh 16-bit seed and the extension ID, not freely shuffled.
	// This fresh profile has no ECH or PSK offers requiring a final position.
	slices.SortStableFunc(ordered, func(a, b extension) int {
		x := codexRustlsExtensionOrderKey(seed, a.id)
		y := codexRustlsExtensionOrderKey(seed, b.id)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
		return 0
	})
	extensions := make([]tls.TLSExtension, len(ordered))
	for i, ext := range ordered {
		extensions[i] = ext.value
	}
	return &tls.ClientHelloSpec{
		TLSVersMin: tls.VersionTLS12, TLSVersMax: tls.VersionTLS13,
		CipherSuites:       []uint16{4866, 4865, 4867, 49196, 49195, 52393, 49200, 49199, 52392, 255},
		CompressionMethods: []byte{0}, Extensions: extensions,
	}
}

// Matches rustls 0.23.45 msgs/handshake.rs low_quality_integer_hash.
func codexRustlsExtensionOrderKey(seed, extensionID uint16) uint32 {
	x := uint32(seed)<<16 | uint32(extensionID)
	x = x + 0x7ed55d16 + (x << 12)
	x = (x ^ 0xc761c23c) ^ (x >> 19)
	x = x + 0x165667b1 + (x << 5)
	x = (x + 0xd3a2646c) ^ (x << 9)
	x = x + 0xfd7046c5 + (x << 3)
	return (x ^ 0xb55a4f09) ^ (x >> 16)
}
