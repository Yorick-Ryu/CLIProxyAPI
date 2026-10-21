package helps

import tls "github.com/refraction-networking/utls"

// codexLinuxHTTPHello reproduces the initial ClientHello captured from official
// Linux musl CLI 0.162.1/0.163.0 with no custom CA environment. This is a wire
// compatibility profile, not an OpenSSL implementation. No ALPN means HTTP/1.1.
func codexLinuxHTTPHello() *tls.ClientHelloSpec {
	return &tls.ClientHelloSpec{
		TLSVersMin: tls.VersionTLS12, TLSVersMax: tls.VersionTLS13,
		CipherSuites:       []uint16{4866, 4867, 4865, 49196, 49200, 159, 52393, 52392, 52394, 49195, 49199, 158, 49188, 49192, 107, 49187, 49191, 103, 49162, 49172, 57, 49161, 49171, 51, 157, 156, 61, 60, 53, 47},
		CompressionMethods: []byte{0},
		Extensions: []tls.TLSExtension{
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateNever},
			&tls.SNIExtension{},
			&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{4588, 29, 23, 30, 24, 25, 256, 257}},
			&tls.SessionTicketExtension{},
			&tls.GenericExtension{Id: 22},
			&tls.ExtendedMasterSecretExtension{},
			&tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []tls.SignatureScheme{2309, 2310, 2308, 1027, 1283, 1539, 2055, 2056, 2074, 2075, 2076, 2057, 2058, 2059, 2052, 2053, 2054, 1025, 1281, 1537, 771, 769, 770, 1026, 1282, 1538}},
			&tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: 4588}, {Group: 29}}},
		},
	}
}
