package tlsfp

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"testing/iotest"

	tls "github.com/bogdanfinn/utls"
)

// goldenMeta is the provenance stamped into test captures.
var goldenMeta = CaptureMeta{
	CapturedAt:  "2026-09-27T10:00:00Z",
	BrowserPath: "/home/u/.cloakbrowser/chromium-151.0.7922.108.6/chrome",
	BrowserVer:  "151.0.7922.108.6",
	Host:        "hh.ru",
}

// goldenBuilder assembles a wire-accurate Chrome-151-shaped ClientHello
// record: GREASE first cipher, GREASE extension first and last, MLKEM+X25519
// key shares, ALPS-new, ECH grease, record size limit, delegated
// credentials — the parameter set a current cloakbrowser chromium sends.
type extCase struct {
	id   uint16
	data []byte
}

func greaseExt(id uint16) extCase { return extCase{id: id, data: []byte{0x00}} }

func u16b(vals ...uint16) []byte {
	var b bytes.Buffer
	b.Write([]byte{byte(len(vals) * 2 >> 8), byte(len(vals) * 2)})
	for _, v := range vals {
		b.Write([]byte{byte(v >> 8), byte(v)})
	}
	return b.Bytes()
}

func protoList(protos ...string) []byte {
	var body bytes.Buffer
	for _, p := range protos {
		body.Write(append([]byte{byte(len(p))}, []byte(p)...))
	}
	return append([]byte{byte(body.Len() >> 8), byte(body.Len())}, body.Bytes()...)
}

func list1B(body []byte) []byte { return append([]byte{byte(len(body))}, body...) }

func buildGoldenHello(t *testing.T) []byte {
	t.Helper()
	mlkemShare := bytes.Repeat([]byte{0xAB}, 1184) // X25519MLKEM768 client share size
	x25519Share := bytes.Repeat([]byte{0xCD}, 32)

	sni := []byte("hh.ru")
	sniBody := append([]byte{0x00, byte((len(sni) + 3) >> 8), byte(len(sni) + 3), 0x00, byte(len(sni))}, sni...)
	echBody := []byte{
		0x00,       // outer CH type
		0x00, 0x01, // kdf: HKDF-SHA256
		0x00, 0x01, // aead: AES-128-GCM
		0x42,       // config id (grease)
		0x00, 0x00, // empty enc
		0x00, 0x90, // payload len 144
	}
	echBody = append(echBody, bytes.Repeat([]byte{0x77}, 144)...)

	var ks bytes.Buffer
	ks.Write([]byte{0x1a, 0x1a, 0x00, 0x01, 0x00}) // grease group, 1-byte share
	ks.Write([]byte{0x11, 0xec, 0x04, 0xA0})       // X25519MLKEM768, len 1184
	ks.Write(mlkemShare)                           //nolint:staticcheck // append reuse safe
	ks.Write([]byte{0x00, 0x1d, 0x00, 0x20})       // X25519, len 32
	ks.Write(x25519Share)                          //nolint:staticcheck // append reuse safe
	keyShareBody := append([]byte{byte(ks.Len() >> 8), byte(ks.Len())}, ks.Bytes()...)

	exts := []extCase{
		greaseExt(0x0a0a),                    // leading GREASE ext
		{extServerName, sniBody},             // server_name
		{extExtMasterSecret, nil},            // extended_master_secret
		{extRenegotiationInfo, []byte{0x00}}, // renegotiation_info: empty list (1-byte len)
		{extSupportedGroups, u16b(0x1a1a, 0x11ec, 0x001d, 0x0017)},
		{extECPointFormats, list1B([]byte{0x00})},
		{extSessionTicket, nil},
		{extApplicationSettingsNew, protoList("h2")},
		{extALPN, protoList("h2", "http/1.1")},
		{extStatusRequest, nil}, // zero-length: TLS 1.3 form
		{extSignatureAlgorithms, u16b(0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601)},
		{extSCT, nil},
		{extKeyShare, keyShareBody},
		{extPSKModes, list1B([]byte{0x01})},
		{extSupportedVersions, list1B([]byte{0x1a, 0x1a, 0x03, 0x04, 0x03, 0x03})},
		{extCompressCertificate, list1B([]byte{0x00, 0x02, 0x00, 0x01})},
		{extDelegatedCredentials, u16b(0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601)},
		{extRecordSizeLimit, []byte{0x40, 0x01}},
		{extECH, echBody},
		greaseExt(0x1a1a), // trailing GREASE ext
	}

	var extBlock bytes.Buffer
	for _, e := range exts {
		extBlock.Write([]byte{byte(e.id >> 8), byte(e.id), byte(len(e.data) >> 8), byte(len(e.data))})
		extBlock.Write(e.data)
	}

	var ciphers bytes.Buffer
	cipherList := []uint16{0x1a1a, 0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0xc013, 0xc014}
	ciphers.Write([]byte{byte(len(cipherList) * 2 >> 8), byte(len(cipherList) * 2)})
	for _, c := range cipherList {
		ciphers.Write([]byte{byte(c >> 8), byte(c)})
	}

	sid := bytes.Repeat([]byte{0x11}, 32)
	hello := append([]byte{
		0x03, 0x03, // legacy version
	}, bytes.Repeat([]byte{0x42}, 32)...) // random
	hello = append(hello, 32)
	hello = append(hello, sid...)
	hello = append(hello, ciphers.Bytes()...)
	hello = append(hello, 0x01, 0x00) // compression: null
	hello = append(hello, byte(extBlock.Len()>>8), byte(extBlock.Len()))
	hello = append(hello, extBlock.Bytes()...)

	hs := append([]byte{0x01, byte(len(hello) >> 16), byte(len(hello) >> 8), byte(len(hello))}, hello...)
	rec := append([]byte{0x16, 0x03, 0x01, byte(len(hs) >> 8), byte(len(hs))}, hs...)
	return rec
}

func goldenSpec(t *testing.T) *Spec {
	t.Helper()
	s, err := ParseClientHello(buildGoldenHello(t), goldenMeta)
	if err != nil {
		t.Fatalf("ParseClientHello: %v", err)
	}
	return s
}

func TestParseClientHelloGolden(t *testing.T) {
	s := goldenSpec(t)

	if s.RecordVersion != 0x0301 || s.HandshakeVer != 0x0303 {
		t.Fatalf("versions: %#04x / %#04x", s.RecordVersion, s.HandshakeVer)
	}
	wantCiphers := []uint16{0x1a1a, 0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0xc013, 0xc014}
	if !u16SetEqual(s.CipherSuites, wantCiphers) || len(s.CipherSuites) != len(wantCiphers) {
		t.Fatalf("ciphers: got %v", s.CipherSuites)
	}
	if !isGREASE16(s.CipherSuites[0]) {
		t.Fatalf("leading cipher must be GREASE, got %#04x", s.CipherSuites[0])
	}
	wantGroups := []uint16{0x1a1a, 0x11ec, 0x001d, 0x0017}
	if !u16SetEqual(s.SupportedGroups, wantGroups) {
		t.Fatalf("groups: got %v", s.SupportedGroups)
	}
	wantSig := []uint16{0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601}
	if !u16SetEqual(s.SigAlgs, wantSig) {
		t.Fatalf("sigalgs: got %v", s.SigAlgs)
	}
	if !strSetEqual(s.ALPN, []string{"h2", "http/1.1"}) {
		t.Fatalf("alpn: %v", s.ALPN)
	}
	wantKS := []uint16{0x1a1a, 0x11ec, 0x001d}
	if !u16SetEqual(s.KeyShareGroups, wantKS) {
		t.Fatalf("key shares: %v", s.KeyShareGroups)
	}
	if !u8SetEqual(s.PskModes, []uint8{0x01}) {
		t.Fatalf("psk modes: %v", s.PskModes)
	}
	if !u16SetEqual(s.SupportedVers, []uint16{0x1a1a, 0x0304, 0x0303}) {
		t.Fatalf("supported versions: %v", s.SupportedVers)
	}
	if !u16SetEqual(s.CertCompAlgs, []uint16{0x0002, 0x0001}) {
		t.Fatalf("cert comp: %v", s.CertCompAlgs)
	}
	if !s.StatusRequestZero || !s.HasSessionTicket || !s.HasECH || s.HasPadding {
		t.Fatalf("flags: zero_status=%v ticket=%v ech=%v padding=%v",
			s.StatusRequestZero, s.HasSessionTicket, s.HasECH, s.HasPadding)
	}
	// GREASE classification on the parsed extension list.
	greaseCount := 0
	for _, e := range s.Extensions {
		if e.Grease {
			greaseCount++
			if e.Name != "GREASE" {
				t.Fatalf("grease ext name: %q", e.Name)
			}
		}
	}
	if greaseCount != 2 {
		t.Fatalf("grease exts: got %d, want 2", greaseCount)
	}
	if s.Host != "hh.ru" || s.BrowserVer != goldenMeta.BrowserVer || s.SchemaVersion != SchemaVersion {
		t.Fatalf("meta not stamped: %+v", s)
	}
}

func TestJA3Golden(t *testing.T) {
	s := goldenSpec(t)

	// Normalized JA3, built independently from the wire order minus GREASE.
	wantJA3 := "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172," +
		"0-23-65281-10-11-35-17613-16-5-13-18-51-45-43-27-34-28-65037," +
		"4588-29-23,0"
	if s.JA3 != wantJA3 {
		t.Fatalf("ja3 normalized:\n got %s\nwant %s", s.JA3, wantJA3)
	}
	if s.JA3MD5 != hex.EncodeToString(md5Sum([]byte(wantJA3))) {
		t.Fatalf("ja3 md5 mismatch")
	}
	if s.JA3Raw == s.JA3 {
		t.Fatalf("raw JA3 must retain GREASE values")
	}
	if !strings.Contains(s.JA3Raw, "6682") {
		t.Fatalf("raw ja3 should contain grease cipher 0x1a1a=%d: %s", 0x1a1a, s.JA3Raw)
	}
}

func TestVerifyReplayMatchesCapture(t *testing.T) {
	s := goldenSpec(t)
	if err := s.Verify(3); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyDetectsStructuralDrift(t *testing.T) {
	s := goldenSpec(t)

	// Drift the expectation away from the raw bytes: the replay is
	// derived from raw_client_hello_hex, so any mutation of the parsed
	// expectation must fail the gate.
	drifted := *s
	drifted.SigAlgs = append([]uint16(nil), s.SigAlgs...)
	drifted.SigAlgs = drifted.SigAlgs[:len(drifted.SigAlgs)-1]
	if err := (&drifted).Verify(1); err == nil {
		t.Fatalf("dropped sigalg must fail verification")
	}

	drifted2 := *s
	drifted2.ALPN = []string{"h2", "http/1.1", "h3"}
	if err := (&drifted2).Verify(1); err == nil {
		t.Fatalf("extra ALPN must fail verification")
	}
}

func TestBuildClientHelloSpecTypes(t *testing.T) {
	raw := buildGoldenHello(t)
	spec, err := buildClientHelloSpec(raw)
	if err != nil {
		t.Fatalf("buildClientHelloSpec: %v", err)
	}
	// With supported_versions present the hello versions come from the ext.
	if spec.TLSVersMin != 0 || spec.TLSVersMax != 0 {
		t.Fatalf("TLSVers must stay 0 with supported_versions: %v/%v", spec.TLSVersMin, spec.TLSVersMax)
	}
	if len(spec.CipherSuites) != 12 {
		t.Fatalf("ciphers after grease->placeholder: %v", spec.CipherSuites)
	}
	if spec.CipherSuites[0] != tls.GREASE_PLACEHOLDER {
		t.Fatalf("leading cipher must be the GREASE placeholder, got %#04x", spec.CipherSuites[0])
	}
	var sawSNI, sawZeroStatus, sawECH, sawKeyShare bool
	for _, e := range spec.Extensions {
		switch ext := e.(type) {
		case *tls.SNIExtension:
			sawSNI = true
			if ext.ServerName != "" {
				t.Fatalf("SNI must be per-connection (empty in spec), got %q", ext.ServerName)
			}
		case *tls.GenericExtension:
			if ext.Id == extStatusRequest && len(ext.Data) == 0 {
				sawZeroStatus = true
			}
		case *tls.GREASEEncryptedClientHelloExtension:
			sawECH = true
		case *tls.KeyShareExtension:
			sawKeyShare = true
			if len(ext.KeyShares) != 3 {
				t.Fatalf("key shares: %d", len(ext.KeyShares))
			}
			if ext.KeyShares[0].Group != tls.GREASE_PLACEHOLDER || len(ext.KeyShares[0].Data) != 1 {
				t.Fatalf("grease share: %+v", ext.KeyShares[0])
			}
			if ext.KeyShares[1].Group != tls.X25519MLKEM768 || ext.KeyShares[1].Data != nil {
				t.Fatalf("mlkem share must be regenerated (nil data): %+v", ext.KeyShares[1])
			}
			if ext.KeyShares[2].Group != tls.X25519 || ext.KeyShares[2].Data != nil {
				t.Fatalf("x25519 share must be regenerated (nil data): %+v", ext.KeyShares[2])
			}
		}
	}
	if !sawSNI || !sawZeroStatus || !sawECH || !sawKeyShare {
		t.Fatalf("missing extensions: sni=%v zero_status=%v ech=%v keyshare=%v",
			sawSNI, sawZeroStatus, sawECH, sawKeyShare)
	}
}

func TestReadClientHelloRecordSegmented(t *testing.T) {
	raw := buildGoldenHello(t)
	got, err := ReadClientHelloRecord(iotest.OneByteReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatalf("segmented read: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("segmented read changed bytes")
	}
	// Non-hello record: application data.
	if _, err := ReadClientHelloRecord(bytes.NewReader([]byte{0x17, 0x03, 0x03, 0x00, 0x02, 1, 2})); err == nil {
		t.Fatalf("non-handshake record must be rejected")
	}
	// Truncated body.
	trunc := raw[:len(raw)-10]
	if _, err := ReadClientHelloRecord(bytes.NewReader(trunc)); err == nil {
		t.Fatalf("truncated record must be rejected")
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	s := goldenSpec(t)
	path := t.TempDir() + "/browser/tls_spec.json"
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.JA3MD5 != s.JA3MD5 || got.BrowserVer != s.BrowserVer {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if !bytes.Equal(mustHex(t, got.RawClientHelloHex), mustHex(t, s.RawClientHelloHex)) {
		t.Fatalf("raw hello mismatch after roundtrip")
	}
	if err := got.Verify(1); err != nil {
		t.Fatalf("loaded spec must verify: %v", err)
	}
}

func TestLoadRejectsSchemaDrift(t *testing.T) {
	s := goldenSpec(t)
	path := t.TempDir() + "/spec.json"
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	broken := strings.Replace(string(mustRead(t, path)),
		`"schema_version": 1`, `"schema_version": 999`, 1)
	if broken == string(mustRead(t, path)) {
		t.Fatalf("schema string not patched")
	}
	if err := writeFile(t, path, broken); err != nil {
		t.Fatalf("write broken: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("schema drift must fail Load")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for name, in := range map[string][]byte{
		"nil":        nil,
		"short":      {0x16, 0x03},
		"not TLS":    {0x17, 0x03, 0x03, 0x00, 0x04, 1, 2, 3, 4},
		"not hello":  {0x16, 0x03, 0x01, 0x00, 0x04, 0x02, 0, 0, 0},
		"trunc body": {0x16, 0x03, 0x01, 0x00, 0x50, 0x01, 0x00, 0x00, 0x4c, 0x03, 0x03},
	} {
		if _, err := ParseClientHello(in, CaptureMeta{}); err == nil {
			t.Fatalf("%s must fail parsing", name)
		}
	}
}

// TestGREASEClassification locks the RFC 8701 value shape detection.
func TestGREASEClassification(t *testing.T) {
	for _, v := range []uint16{0x0a0a, 0x1a1a, 0x2a2a, 0x3a3a, 0x5a5a, 0xfafa} {
		if !isGREASE16(v) {
			t.Fatalf("%#04x is GREASE", v)
		}
	}
	for _, v := range []uint16{0x0000, 0x001d, 0x1301, 0xfe0d, 0xff01, 0x0a0b, 0xaa0a} {
		if isGREASE16(v) {
			t.Fatalf("%#04x is NOT GREASE", v)
		}
	}
}

func md5Sum(b []byte) []byte {
	s := md5.Sum(b)
	return s[:]
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	return b
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func writeFile(t *testing.T, path, content string) error {
	t.Helper()
	return os.WriteFile(path, []byte(content), 0o600)
}
