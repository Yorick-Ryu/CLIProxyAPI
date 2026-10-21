package helps

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"

	tls "github.com/refraction-networking/utls"
)

// CodexWebsocketTLSHandshake matches the initial rustls ClientHello observed in
// Codex CLI 0.162.1 and 0.163.0. This is not a replacement for the rustls record
// layer. No session cache is shared between accounts and no ALPN is offered.
func CodexWebsocketTLSHandshake(ctx context.Context, raw net.Conn, serverName string) (net.Conn, error) {
	return codexWebsocketTLSHandshake(ctx, raw, &tls.Config{ServerName: serverName})
}

func codexWebsocketTLSHandshake(ctx context.Context, raw net.Conn, cfg *tls.Config) (net.Conn, error) {
	conn := tls.UClient(raw, cfg, tls.HelloCustom)
	if err := conn.ApplyPreset(codexWebsocketHello()); err != nil {
		return nil, fmt.Errorf("codex websocket TLS profile: %w", err)
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("codex websocket TLS handshake: %w", err)
	}
	return conn, nil
}

func codexWebsocketHello() *tls.ClientHelloSpec {
	extensions := []tls.TLSExtension{
		&tls.SNIExtension{},
		&tls.StatusRequestExtension{},
		&tls.SupportedCurvesExtension{Curves: []tls.CurveID{4588, 29, 23, 24}},
		&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
		&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{1283, 1027, 1539, 2055, 2054, 2053, 2052, 1537, 1281, 1025, 2308, 2309, 2310}},
		&tls.ExtendedMasterSecretExtension{},
		&tls.SessionTicketExtension{},
		&tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
		&tls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
		&tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: 4588}, {Group: 29}}},
	}
	// Rustls randomizes extension order; do not pin one captured JA3 ordering.
	rand.Shuffle(len(extensions), func(i, j int) { extensions[i], extensions[j] = extensions[j], extensions[i] })
	return &tls.ClientHelloSpec{
		TLSVersMin: tls.VersionTLS12, TLSVersMax: tls.VersionTLS13,
		CipherSuites:       []uint16{4866, 4865, 4867, 49196, 49195, 52393, 49200, 49199, 52392, 255},
		CompressionMethods: []byte{0}, Extensions: extensions,
	}
}
