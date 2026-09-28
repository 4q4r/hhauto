// Package tlsfp (T37 v3) carries the 1:1 TLS-fingerprint replay layer:
// a ClientHello captured byte-for-byte from the cloakbrowser chromium is
// turned into a uTLS ClientHelloSpec that every hh connection then sends.
// The design goal is parity, not approximation — the captured record
// bytes are the single source of truth and are re-parsed fresh for every
// handshake (uTLS extension objects carry per-connection state, so specs
// must never be shared across connections; see ApplyPreset docs).
//
// Ground truth chain:
//
//	browser hello bytes (tap) → ParseClientHello → Spec (JSON cache)
//	  → Spec.ClientHelloID().SpecFactory → utls.FromRaw → fresh spec per conn
//
// Why the replay stays faithful without replaying replayable material:
//   - key shares: uTLS discards captured key-share data and generates a
//     fresh key pair per connection — exactly what the browser does;
//     replaying captured ephemeral keys would itself be a replay signal;
//   - GREASE: captured GREASE values collapse to the uTLS GREASE
//     placeholder and are re-randomized per connection per RFC 8701,
//     like the browser;
//   - ECH GREASE: the uTLS GREASE-ECH extension regenerates a fresh
//     random payload per connection;
//   - extension order: Chrome permutes extensions on every connection
//     since ~v110 (Fastly, 2023); the transport enables uTLS's
//     ShuffleChromeTLSExtensions (GREASE/padding/PSK stay positionally
//     fixed, the rest shuffles) so the replayed order distribution
//     matches the browser instead of freezing one captured order.
//
// Verification compares the GREASE-normalized JA3 (the official JA3 spec
// ignores GREASE; JA4 mandates the same) plus the sorted parameter
// multisets, so it is insensitive to GREASE re-randomization and to the
// extension shuffle while still failing on any structural drift.
package tlsfp

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tls "github.com/bogdanfinn/utls"
)

// SchemaVersion guards the cached JSON against silent format drift.
// v2 (T50) adds user_agent: the capture is the single source of truth for
// the UA as well, so v1 caches fail Load and are recaptured automatically.
const SchemaVersion = 2

// ExtInfo is one ClientHello extension as seen on the wire.
type ExtInfo struct {
	ID     uint16 `json:"id"`
	Name   string `json:"name"`
	Grease bool   `json:"grease"`
}

// CaptureMeta carries the provenance fields stamped into a Spec.
type CaptureMeta struct {
	CapturedAt  string // RFC 3339 UTC
	BrowserPath string
	BrowserVer  string
	Host        string // SNI host the hello was captured for
	UserAgent   string // navigator.userAgent of the capturing browser
}

// Spec is the captured browser ClientHello, both structurally parsed
// (for introspection and verification) and as raw record bytes (the
// replay ground truth). JSON-serializable; Save/Load persist it under
// data_dir.
type Spec struct {
	SchemaVersion int    `json:"schema_version"`
	CapturedAt    string `json:"captured_at"`
	BrowserPath   string `json:"browser_path"`
	BrowserVer    string `json:"browser_version"`
	Host          string `json:"host"`

	// RawClientHelloHex is the full first TLS record (record header +
	// handshake) exactly as the browser sent it. It feeds
	// ClientHelloSpec.FromRaw and is therefore the replay ground truth;
	// every other field in this struct is derived from it.
	RawClientHelloHex string `json:"raw_client_hello_hex"`

	// UserAgent is the navigator.userAgent of the capturing browser,
	// captured in the same session as the hello: the spec is the single
	// source of truth for the UA too (T50).
	UserAgent string `json:"user_agent"`

	RecordVersion     uint16    `json:"record_version"` // TLS record layer version
	HandshakeVer      uint16    `json:"handshake_ver"`  // legacy hello version (0x0303)
	CipherSuites      []uint16  `json:"cipher_suites"`  // as-wire order, GREASE included
	Compression       []uint8   `json:"compression_methods"`
	Extensions        []ExtInfo `json:"extensions"` // as-wire order
	SupportedGroups   []uint16  `json:"supported_groups"`
	PointFormats      []uint8   `json:"ec_point_formats"`
	SigAlgs           []uint16  `json:"signature_algorithms"`
	ALPN              []string  `json:"alpn"`
	KeyShareGroups    []uint16  `json:"key_share_groups"`
	PskModes          []uint8   `json:"psk_key_exchange_modes"`
	SupportedVers     []uint16  `json:"supported_versions"`
	CertCompAlgs      []uint16  `json:"cert_compression_algs"`
	StatusRequestZero bool      `json:"status_request_zero_len"` // TLS 1.3 form (BoringSSL) vs the TLS 1.2 OCSP form
	HasSessionTicket  bool      `json:"has_session_ticket"`
	HasECH            bool      `json:"has_ech_grease"`
	HasPadding        bool      `json:"has_padding"`

	JA3Raw string `json:"ja3_raw"` // classic JA3 string, GREASE as-seen
	JA3    string `json:"ja3"`     // GREASE-normalized JA3 string
	JA3MD5 string `json:"ja3_md5"` // md5(JA3) — the normalized fingerprint
}

// DefaultPath returns the canonical cache location inside a data dir.
func DefaultPath(dataDir string) string {
	return filepath.Join(dataDir, "browser", "tls_spec.json")
}

// RawClientHello returns the captured record bytes.
func (s *Spec) RawClientHello() ([]byte, error) {
	return hex.DecodeString(s.RawClientHelloHex)
}

// ClientHelloID builds the uTLS client-hello identity whose SpecFactory
// re-parses the captured record and rebuilds a typed ClientHelloSpec on
// every call. A fresh spec per connection is mandatory: uTLS extension
// objects are stateful and sharing them across concurrent handshakes is
// a data race (see the ApplyPreset documentation).
//
// Two deviations from the captured bytes are deliberate and keep the
// replay faithful at the same time:
//   - SNI: the captured server_name value is dropped; the empty
//     SNIExtension lets the per-connection server name flow in (the
//     browser also adapts SNI per navigation);
//   - everything replayable (key shares, GREASE, ECH grease, session
//     id) is regenerated by uTLS per connection.
func (s *Spec) ClientHelloID() tls.ClientHelloID {
	raw, err := s.RawClientHello()
	return tls.ClientHelloID{
		Client:  "cloakbrowser",
		Version: s.BrowserVer,
		SpecFactory: func() (tls.ClientHelloSpec, error) {
			if err != nil {
				return tls.ClientHelloSpec{}, fmt.Errorf("tlsfp: raw hello unavailable: %w", err)
			}
			spec, berr := buildClientHelloSpec(raw)
			if berr != nil {
				return tls.ClientHelloSpec{}, fmt.Errorf("tlsfp: build spec: %w", berr)
			}
			return spec, nil
		},
	}
}

// Save persists the spec as JSON at path (typically DefaultPath(dataDir)).
// The write is atomic (temp file in the same directory + rename): the
// engine recaptures at startup, so an interrupted write must never leave
// a truncated spec behind.
func (s *Spec) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("tlsfp: encode spec: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("tlsfp: create spec dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tls_spec-*.json")
	if err != nil {
		return fmt.Errorf("tlsfp: create temp spec: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("tlsfp: write spec: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("tlsfp: write spec: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("tlsfp: replace spec: %w", err)
	}
	return nil
}

// Load reads a previously captured spec. A schema-version mismatch is an
// error: a stale or foreign cache must never be replayed against hh.
func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tlsfp: read spec: %w", err)
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("tlsfp: decode spec: %w", err)
	}
	if s.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("tlsfp: spec schema %d, want %d (re-run the tls-probe command)", s.SchemaVersion, SchemaVersion)
	}
	if s.RawClientHelloHex == "" {
		return nil, errors.New("tlsfp: spec has no raw client hello")
	}
	if s.UserAgent == "" {
		return nil, errors.New("tlsfp: spec has no user_agent (re-run the tls-probe capture)")
	}
	if _, err := s.RawClientHello(); err != nil {
		return nil, fmt.Errorf("tlsfp: spec raw hello: %w", err)
	}
	return &s, nil
}

// Verify replays the captured hello through a real uTLS client against a
// local byte tap and checks that every iteration produces a hello
// structurally identical to the capture (normalized JA3 + sorted
// parameter multisets). iterations bounds how many fresh handshakes are
// exercised; values <= 0 mean 2.
func (s *Spec) Verify(iterations int) error {
	if iterations <= 0 {
		iterations = 2
	}
	id := s.ClientHelloID()
	for i := 0; i < iterations; i++ {
		if err := s.verifyOnce(id); err != nil {
			return fmt.Errorf("tlsfp: verify iteration %d/%d: %w", i+1, iterations, err)
		}
	}
	return nil
}

func (s *Spec) verifyOnce(id tls.ClientHelloID) error {
	helloSide, tapSide := net.Pipe()
	type result struct {
		got *Spec
		err error
	}
	res := make(chan result, 1)
	go func() {
		defer helloSide.Close() //nolint:errcheck // pipe end
		raw, err := ReadClientHelloRecord(helloSide)
		if err != nil {
			res <- result{err: err}
			return
		}
		got, err := ParseClientHello(raw, CaptureMeta{})
		res <- result{got: got, err: err}
	}()

	// The client handshake is expected to fail once the tap closes after
	// reading the hello — the bytes on the wire are the assertion target,
	// not the completed handshake.
	go func() {
		defer tapSide.Close()                                            //nolint:errcheck // pipe end
		cfg := &tls.Config{ServerName: s.Host, InsecureSkipVerify: true} //nolint:gosec // local byte tap, no secrets
		uconn := tls.UClient(tapSide, cfg, id, true, false, true)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = uconn.HandshakeContext(ctx)
	}()

	r := <-res
	if r.err != nil {
		return fmt.Errorf("read replayed hello: %w", r.err)
	}
	return compareSpecs(r.got, s)
}

// verifyDigest hashes every GREASE-normalized, sorted parameter list of
// the hello into one order-insensitive digest. It is the comparison key
// for replay verification: GREASE re-randomization and the extension
// shuffle must not flip it, any structural drift must.
func (s *Spec) verifyDigest() string {
	h := sha256.New()
	write := func(label string, vals []uint16) {
		sorted := append([]uint16(nil), vals...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		var row strings.Builder
		row.WriteString(label)
		row.WriteString(":")
		for _, v := range sorted {
			row.WriteString(strconv.FormatUint(uint64(v), 10))
			row.WriteString(",")
		}
		row.WriteString("\n")
		_, _ = io.WriteString(h, row.String()) // sha256 digest never fails
	}
	write("c", filterGREASE16(s.CipherSuites, true))
	// server_name is excluded: SNI is per-connection by design (the
	// replay drops the captured value for the dial-time host).
	exts := make([]uint16, 0, len(s.Extensions))
	for _, e := range s.Extensions {
		if e.ID != extServerName {
			exts = append(exts, e.ID)
		}
	}
	write("x", filterGREASE16(exts, true))
	write("g", filterGREASE16(s.SupportedGroups, true))
	write("s", filterGREASE16(s.SigAlgs, true))
	write("k", filterGREASE16(s.KeyShareGroups, true))
	write("v", filterGREASE16(s.SupportedVers, true))
	write("a", s.CertCompAlgs)
	strs := append([]string(nil), s.ALPN...)
	sort.Strings(strs)
	for _, a := range strs {
		_, _ = fmt.Fprintf(h, "alpn:%s\n", a) // sha256 digest never fails
	}
	_, _ = fmt.Fprintf(h, "rec:%d\nhs:%d\nzst:%v\ntkt:%v\nech:%v\npad:%v\n",
		s.RecordVersion, s.HandshakeVer, s.StatusRequestZero, s.HasSessionTicket, s.HasECH, s.HasPadding)
	return hex.EncodeToString(h.Sum(nil))
}

// MatchesWire parses a wire-captured ClientHello record and checks it
// structurally against this spec (GREASE-normalized, order-insensitive —
// see Verify). It is the shared assertion for the probe self-check and
// the transport integration tests: whatever bytes a connection actually
// produced must match the capture.
func (s *Spec) MatchesWire(raw []byte) error {
	got, err := ParseClientHello(raw, CaptureMeta{})
	if err != nil {
		return fmt.Errorf("tlsfp: match wire hello: %w", err)
	}
	return compareSpecs(got, s)
}

// compareSpecs asserts structural equality of two hello parses. All
// comparisons are GREASE-normalized and order-insensitive (the browser —
// and the faithful replay — re-randomize GREASE and permute extension
// order per connection). Parameters outside JA3 (signature algorithms,
// ALPN, key-share groups, PSK modes, cert compression, record version)
// get explicit checks.
func compareSpecs(got, want *Spec) error {
	if got.RecordVersion != want.RecordVersion {
		return fmt.Errorf("record version %#04x, want %#04x", got.RecordVersion, want.RecordVersion)
	}
	if !u16SetEqual(filterGREASE16(got.CipherSuites, true), filterGREASE16(want.CipherSuites, true)) {
		return fmt.Errorf("ciphers %v, want %v", got.CipherSuites, want.CipherSuites)
	}
	if !extSetEqual(got, want) {
		return fmt.Errorf("extensions %v, want %v", extIDs(got), extIDs(want))
	}
	if !u16SetEqual(filterGREASE16(got.SupportedGroups, true), filterGREASE16(want.SupportedGroups, true)) {
		return fmt.Errorf("groups %v, want %v", got.SupportedGroups, want.SupportedGroups)
	}
	if !u16SetEqual(filterGREASE16(got.SigAlgs, true), filterGREASE16(want.SigAlgs, true)) {
		return fmt.Errorf("signature algorithms %v, want %v", got.SigAlgs, want.SigAlgs)
	}
	if !strSetEqual(got.ALPN, want.ALPN) {
		return fmt.Errorf("ALPN %v, want %v", got.ALPN, want.ALPN)
	}
	if !u16SetEqual(filterGREASE16(got.KeyShareGroups, true), filterGREASE16(want.KeyShareGroups, true)) {
		return fmt.Errorf("key share groups %v, want %v", got.KeyShareGroups, want.KeyShareGroups)
	}
	if !u8SetEqual(got.PskModes, want.PskModes) {
		return fmt.Errorf("psk modes %v, want %v", got.PskModes, want.PskModes)
	}
	if !u16SetEqual(got.CertCompAlgs, want.CertCompAlgs) {
		return fmt.Errorf("cert compression %v, want %v", got.CertCompAlgs, want.CertCompAlgs)
	}
	if !u16SetEqual(filterGREASE16(got.SupportedVers, true), filterGREASE16(want.SupportedVers, true)) {
		return fmt.Errorf("supported versions %v, want %v", got.SupportedVers, want.SupportedVers)
	}
	// The digest is the order-insensitive catch-all on top of the
	// per-field checks: the browser (and the faithful replay) permutes
	// extension order per connection, so the order-sensitive JA3 strings
	// are never compared directly.
	if got.verifyDigest() != want.verifyDigest() {
		return fmt.Errorf("structural digest mismatch: got ja3 %s, want ja3 %s", got.JA3, want.JA3)
	}
	return nil
}

func extIDs(s *Spec) []uint16 {
	out := make([]uint16, 0, len(s.Extensions))
	for _, e := range s.Extensions {
		out = append(out, e.ID)
	}
	return out
}

// extSetEqual compares the extension id sets excluding GREASE and the
// server_name extension (SNI is per-connection by design — the replay
// drops the captured value and lets the dial-time host flow in).
func extSetEqual(got, want *Spec) bool {
	return u16SetEqual(
		filterGREASE16(removeSNI(extIDs(got)), true),
		filterGREASE16(removeSNI(extIDs(want)), true),
	)
}

func removeSNI(ids []uint16) []uint16 {
	out := make([]uint16, 0, len(ids))
	for _, id := range ids {
		if id != extServerName {
			out = append(out, id)
		}
	}
	return out
}

// SniffClientHelloRecord reads one full TLS record from r without
// consuming more than the record itself. It returns the record bytes
// verbatim and whether the record is a ClientHello handshake message.
// Unlike ReadClientHelloRecord it never errors on non-TLS payloads —
// the tap relay must forward whatever the client sends, byte-exact.
func SniffClientHelloRecord(r io.Reader) (raw []byte, isHello bool, err error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, false, fmt.Errorf("read record header: %w", err)
	}
	n := int(header[3])<<8 | int(header[4])
	if header[0] != 0x16 || n <= 0 || n > maxRecordLen {
		return header, false, nil
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return header, false, nil // partial record: forward what was read
	}
	rec := append(header, body...)
	return rec, len(rec) >= 6+40 && rec[5] == 0x01, nil
}

// ReadClientHelloRecord reads exactly one full TLS record from r and
// returns it verbatim (5-byte header included — the FromRaw input form),
// rejecting anything that is not a ClientHello handshake record. It
// tolerates arbitrary TCP segmentation.
func ReadClientHelloRecord(r io.Reader) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("read record header: %w", err)
	}
	if header[0] != 0x16 {
		return nil, fmt.Errorf("tlsfp: record type %#02x, want handshake (0x16)", header[0])
	}
	n := int(header[3])<<8 | int(header[4])
	if n <= 0 || n > maxRecordLen {
		return nil, fmt.Errorf("tlsfp: implausible record length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("read record body: %w", err)
	}
	rec := append(header, body...)
	if len(rec) < 6+40 || rec[5] != 0x01 {
		return nil, errors.New("tlsfp: not a ClientHello handshake message")
	}
	return rec, nil

}

const maxRecordLen = 1 << 15 // 32 KiB: a ClientHello is far below this

// --- wire parsing -------------------------------------------------------

var errNotClientHello = errors.New("tlsfp: not a TLS ClientHello record")

// ParseClientHello decodes the full first TLS record (5-byte record
// header included — the exact bytes a tap captures) into a Spec. Parsing
// is strict: GREASE classification, parameter lists and the normalized
// JA3 are all derived from the same bytes the replay will send.
func ParseClientHello(raw []byte, meta CaptureMeta) (*Spec, error) {
	s := &Spec{
		SchemaVersion:     SchemaVersion,
		CapturedAt:        meta.CapturedAt,
		BrowserPath:       meta.BrowserPath,
		BrowserVer:        meta.BrowserVer,
		Host:              meta.Host,
		UserAgent:         meta.UserAgent,
		RawClientHelloHex: hex.EncodeToString(raw),
	}
	if len(raw) < 6 || raw[0] != 0x16 {
		return nil, errNotClientHello
	}
	s.RecordVersion = be16(raw[1:])
	recLen := int(be16(raw[3:]))
	if len(raw) < 5+recLen || recLen < 4 || raw[5] != 0x01 {
		return nil, errNotClientHello
	}
	// Handshake header: type(1) len(3) — bounded by the record length.
	body := raw[9 : 5+recLen]
	if len(body) < 2+32+1 {
		return nil, errNotClientHello
	}
	s.HandshakeVer = be16(body)
	body = body[2:][32:] // legacy version + 32-byte random

	sidLen := int(body[0])
	rest, ok := cut(body[1:], sidLen)
	if !ok {
		return nil, errNotClientHello
	}
	csLen := int(be16(rest))
	suites, ok := take(rest[2:], csLen)
	if !ok || csLen%2 != 0 {
		return nil, errNotClientHello
	}
	for len(suites) >= 2 {
		s.CipherSuites = append(s.CipherSuites, be16(suites))
		suites = suites[2:]
	}
	compLen := int(rest[2+csLen])
	comp, ok := take(rest[3+csLen:], compLen)
	if !ok {
		return nil, errNotClientHello
	}
	s.Compression = append(s.Compression, comp...)

	if err := iterateExtensions(raw, func(id uint16, data []byte) error {
		s.parseExtension(id, data)
		return nil
	}); err != nil {
		return nil, err
	}
	s.finish()
	return s, nil
}

// parseExtension extracts structured parameter lists from one extension
// body. Everything is stored as-wire (GREASE included); the normalized
// JA3 is where GREASE is dropped, mirroring the official specs.
func (s *Spec) parseExtension(id uint16, data []byte) {
	s.Extensions = append(s.Extensions, ExtInfo{ID: id, Name: extensionName(id), Grease: isGREASE16(id)})
	switch id {
	case extStatusRequest:
		s.StatusRequestZero = len(data) == 0
	case extSupportedGroups:
		s.SupportedGroups = u16PrefixedList(data)
	case extSignatureAlgorithms:
		s.SigAlgs = u16PrefixedList(data)
	case extALPN:
		if protos, err := alpnProtocols(data); err == nil {
			s.ALPN = protos
		}
	case extPadding:
		s.HasPadding = true
	case extCompressCertificate:
		s.CertCompAlgs = u8PrefixedU16List(data)
	case extSupportedVersions:
		s.SupportedVers = u8PrefixedU16List(data)
	case extPSKModes:
		if list, ok := u8Body(data); ok {
			s.PskModes = append(s.PskModes, list...)
		}
	case extKeyShare:
		s.KeyShareGroups = keyShareGroups(data)
	case extECPointFormats:
		if list, ok := u8Body(data); ok {
			s.PointFormats = append(s.PointFormats, list...)
		}
	case extSessionTicket:
		s.HasSessionTicket = true
	case extECH:
		s.HasECH = true
	}
}

// u16PrefixedList parses a 2-byte-length-prefixed list of uint16 bodies
// (supported_groups, signature_algorithms).
func u16PrefixedList(data []byte) []uint16 {
	body, ok := u16Body(data)
	if !ok {
		return nil
	}
	return u16Slice(body)
}

// u8PrefixedU16List parses a 1-byte-length-prefixed list of uint16
// (supported_versions, RFC 8879 compress_certificate).
func u8PrefixedU16List(data []byte) []uint16 {
	body, ok := u8Body(data)
	if !ok {
		return nil
	}
	return u16Slice(body)
}

func u16Slice(body []byte) []uint16 {
	out := make([]uint16, 0, len(body)/2)
	for len(body) >= 2 {
		out = append(out, be16(body))
		body = body[2:]
	}
	return out
}

// keyShareGroups extracts the advertised key-share group ids.
func keyShareGroups(data []byte) []uint16 {
	list, ok := u16Body(data)
	if !ok {
		return nil
	}
	var out []uint16
	for len(list) >= 4 {
		g := be16(list)
		l := int(be16(list[2:]))
		if len(list) < 4+l {
			break
		}
		out = append(out, g)
		list = list[4+l:]
	}
	return out
}

// finish derives the JA3 strings and the normalized hash once parsing is
// complete.
func (s *Spec) finish() {
	exts := make([]uint16, len(s.Extensions))
	for i, e := range s.Extensions {
		exts[i] = e.ID
	}
	s.JA3Raw = ja3String(s.HandshakeVer, s.CipherSuites, exts, s.SupportedGroups, s.PointFormats, false)
	s.JA3 = ja3String(s.HandshakeVer, s.CipherSuites, exts, s.SupportedGroups, s.PointFormats, true)
	sum := md5.Sum([]byte(s.JA3))
	s.JA3MD5 = hex.EncodeToString(sum[:])
}

// --- JA3 ----------------------------------------------------------------

// ja3String renders the JA3 string "SSLVersion,Ciphers,Extensions,
// Curves,PointFormats" (decimal values, "-"-delimited lists). The
// version field is the hello's legacy version, as in every classic JA3
// implementation. normalized=true drops GREASE values from every list —
// the official JA3 ignores GREASE and JA4 mandates it (RFC 8701
// re-randomizes GREASE per connection).
func ja3String(ver uint16, ciphers, exts, curves []uint16, points []uint8, normalized bool) string {
	c := filterGREASE16(ciphers, normalized)
	e := filterGREASE16(exts, normalized)
	g := filterGREASE16(curves, normalized)
	return joinJA3(ver, c, e, g, points)
}

func joinJA3(ver uint16, ciphers, exts, curves []uint16, points []uint8) string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(uint64(ver), 10))
	writeJA3List16(&b, ciphers)
	writeJA3List16(&b, exts)
	writeJA3List16(&b, curves)
	// Point formats: empty list renders as "" (classic JA3 behavior).
	b.WriteString(",")
	for i, p := range points {
		if i > 0 {
			b.WriteString("-")
		}
		b.WriteString(strconv.FormatUint(uint64(p), 10))
	}
	return b.String()
}

func writeJA3List16(b *strings.Builder, vals []uint16) {
	b.WriteString(",")
	for i, v := range vals {
		if i > 0 {
			b.WriteString("-")
		}
		b.WriteString(strconv.FormatUint(uint64(v), 10))
	}
}

// --- extension constants and names --------------------------------------

// ClientHello extension IDs the parser reports in structured form.
const (
	extServerName             = 0x0000
	extStatusRequest          = 0x0005
	extSupportedGroups        = 0x000a
	extECPointFormats         = 0x000b
	extSignatureAlgorithms    = 0x000d
	extALPN                   = 0x0010
	extSCT                    = 0x0012
	extPadding                = 0x0015
	extEncryptThenMAC         = 0x0016
	extExtMasterSecret        = 0x0017
	extCompressCertificate    = 0x001b
	extRecordSizeLimit        = 0x001c
	extDelegatedCredentials   = 0x0022
	extSessionTicket          = 0x0023
	extPSK                    = 0x0029
	extSupportedVersions      = 0x002b
	extPSKModes               = 0x002d
	extKeyShare               = 0x0033
	extRenegotiationInfo      = 0xff01 // renegotiation_info
	extApplicationSettings    = 0x4469
	extApplicationSettingsNew = 0x44cd
	extECH                    = 0xfe0d
)

var extensionNames = map[uint16]string{
	extServerName:             "server_name",
	extStatusRequest:          "status_request",
	extSupportedGroups:        "supported_groups",
	extECPointFormats:         "ec_point_formats",
	extSignatureAlgorithms:    "signature_algorithms",
	extALPN:                   "alpn",
	extSCT:                    "signed_cert_timestamp",
	extPadding:                "padding",
	extEncryptThenMAC:         "encrypt_then_mac",
	extExtMasterSecret:        "extended_master_secret",
	extCompressCertificate:    "compress_certificate",
	extRecordSizeLimit:        "record_size_limit",
	extDelegatedCredentials:   "delegated_credentials",
	extSessionTicket:          "session_ticket",
	extPSK:                    "pre_shared_key",
	extSupportedVersions:      "supported_versions",
	extPSKModes:               "psk_key_exchange_modes",
	extKeyShare:               "key_share",
	extApplicationSettings:    "application_settings",
	extApplicationSettingsNew: "application_settings_new",
	extECH:                    "encrypted_client_hello",
	extRenegotiationInfo:      "renegotiation_info",
}

func extensionName(id uint16) string {
	if isGREASE16(id) {
		return "GREASE"
	}
	if n, ok := extensionNames[id]; ok {
		return n
	}
	return fmt.Sprintf("ext_%d", id)
}

// --- small helpers ------------------------------------------------------

// isGREASE16 reports whether v is a GREASE value (RFC 8701: 0xXaXa where
// both nibbles of both bytes are 'a').
func isGREASE16(v uint16) bool {
	return v&0x0f0f == 0x0a0a && v>>8 == v&0xff
}

func filterGREASE16(vals []uint16, normalized bool) []uint16 {
	if !normalized {
		return vals
	}
	out := make([]uint16, 0, len(vals))
	for _, v := range vals {
		if !isGREASE16(v) {
			out = append(out, v)
		}
	}
	return out
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

func cut(b []byte, n int) (rest []byte, ok bool) {
	if len(b) < n {
		return nil, false
	}
	return b[n:], true
}

// take returns the first n bytes of b (b must be at least that long).
func take(b []byte, n int) (body []byte, ok bool) {
	if len(b) < n {
		return nil, false
	}
	return b[:n], true
}

// u16Body strips the 2-byte list length and returns the list body.
func u16Body(data []byte) (body []byte, ok bool) {
	if len(data) < 2 {
		return nil, false
	}
	return take(data[2:], int(be16(data)))
}

// u8Body strips the 1-byte list length and returns the list body.
func u8Body(data []byte) (body []byte, ok bool) {
	if len(data) < 1 {
		return nil, false
	}
	return take(data[1:], int(data[0]))
}

func u16SetEqual(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]uint16(nil), a...)
	y := append([]uint16(nil), b...)
	sort.Slice(x, func(i, j int) bool { return x[i] < x[j] })
	sort.Slice(y, func(i, j int) bool { return y[i] < y[j] })
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func u8SetEqual(a, b []uint8) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]uint8(nil), a...)
	y := append([]uint8(nil), b...)
	sort.Slice(x, func(i, j int) bool { return x[i] < x[j] })
	sort.Slice(y, func(i, j int) bool { return y[i] < y[j] })
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func strSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// --- spec building (the replay path) -------------------------------------

// buildClientHelloSpec reconstructs a typed uTLS ClientHelloSpec from the
// captured record bytes. Every extension present in the capture must map
// onto a typed uTLS extension; unknown IDs fail loudly instead of
// degrading into generic mimicry — a capture that cannot be replayed
// faithfully must never be used against hh.
//
// GREASE values (ciphers, extension IDs, groups, versions, key-share
// entries) become the uTLS GREASE placeholder and are re-randomized per
// connection; key-share data and the session id are regenerated per
// connection by uTLS; the SNI value is intentionally dropped (see
// ClientHelloID).
func buildClientHelloSpec(raw []byte) (tls.ClientHelloSpec, error) {
	spec := tls.ClientHelloSpec{}
	var versSeen, keyShareSeen bool
	err := iterateExtensions(raw, func(id uint16, data []byte) error {
		ext, err := buildExtension(id, data)
		if err != nil {
			return err
		}
		if id == extKeyShare {
			keyShareSeen = true
		}
		if id == extSupportedVersions {
			versSeen = true
		}
		spec.Extensions = append(spec.Extensions, ext)
		return nil
	})
	if err != nil {
		return tls.ClientHelloSpec{}, err
	}
	spec.CipherSuites, err = capturedCipherSuites(raw)
	if err != nil {
		return tls.ClientHelloSpec{}, err
	}
	if !keyShareSeen {
		return tls.ClientHelloSpec{}, errors.New("tlsfp: capture has no key_share extension")
	}
	if !versSeen {
		// FromRaw semantics: without supported_versions the hello versions
		// come from the record/handshake header.
		spec.TLSVersMin = tls.VersionTLS10
		spec.TLSVersMax = tls.VersionTLS12
	}
	// With supported_versions present TLSVersMin/Max stay 0 — the
	// extension drives version negotiation, mirroring FromRaw.
	return spec, nil
}

// capturedCipherSuites re-extracts the cipher list from the record,
// mapping GREASE entries onto the uTLS placeholder.
func capturedCipherSuites(raw []byte) ([]uint16, error) {
	if len(raw) < 6 || raw[0] != 0x16 {
		return nil, errNotClientHello
	}
	recLen := int(be16(raw[3:]))
	if len(raw) < 5+recLen || recLen < 4 || raw[5] != 0x01 {
		return nil, errNotClientHello
	}
	body := raw[9 : 5+recLen]
	if len(body) < 2+32+1 {
		return nil, errNotClientHello
	}
	body = body[2:][32:] // legacy version + random
	sidLen := int(body[0])
	rest, ok := cut(body[1:], sidLen)
	if !ok {
		return nil, errNotClientHello
	}
	csLen := int(be16(rest))
	suites, ok := take(rest[2:], csLen)
	if !ok || csLen%2 != 0 {
		return nil, errNotClientHello
	}
	out := make([]uint16, 0, csLen/2)
	for len(suites) >= 2 {
		out = append(out, greased(be16(suites)))
		suites = suites[2:]
	}
	return out, nil
}

// buildExtension maps one captured extension onto its typed uTLS writer.
func buildExtension(id uint16, data []byte) (tls.TLSExtension, error) {
	switch {
	case isGREASE16(id):
		return &tls.UtlsGREASEExtension{}, nil
	}
	switch id {
	case extServerName:
		// Per-connection SNI: the empty extension is filled from the
		// dial-time server name by ApplyPreset, like every shipped
		// tls-client browser profile.
		return &tls.SNIExtension{}, nil
	case extSupportedGroups:
		curves, err := buildCurves(data)
		if err != nil {
			return nil, err
		}
		return &tls.SupportedCurvesExtension{Curves: curves}, nil
	case extSignatureAlgorithms:
		algs, err := buildSigAlgs(data)
		if err != nil {
			return nil, err
		}
		return &tls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: algs}, nil
	case extALPN:
		protos, err := alpnProtocols(data)
		if err != nil {
			return nil, err
		}
		return &tls.ALPNExtension{AlpnProtocols: protos}, nil
	case extCompressCertificate:
		algs, err := buildCertCompAlgs(data)
		if err != nil {
			return nil, err
		}
		return &tls.UtlsCompressCertExtension{Algorithms: algs}, nil
	case extSupportedVersions:
		vers, err := buildVersions(data)
		if err != nil {
			return nil, err
		}
		return &tls.SupportedVersionsExtension{Versions: vers}, nil
	case extPSKModes:
		list, ok := u8Body(data)
		if !ok {
			return nil, fmt.Errorf("tlsfp: bad psk_key_exchange_modes body")
		}
		return &tls.PSKKeyExchangeModesExtension{Modes: append([]uint8(nil), list...)}, nil
	case extKeyShare:
		shares, err := buildKeyShares(data)
		if err != nil {
			return nil, err
		}
		return &tls.KeyShareExtension{KeyShares: shares}, nil
	}
	return buildExtensionLate(id, data)
}

// buildExtensionLate maps the second half of the extension vocabulary
// (post-key-share cases plus the catch-all replay).
func buildExtensionLate(id uint16, data []byte) (tls.TLSExtension, error) {
	switch id {
	case extStatusRequest:
		if len(data) == 0 {
			// RFC 8446 §4.3.2 zero-length form (what BoringSSL sends in
			// TLS 1.3). GenericExtension marshals id + empty data verbatim;
			// the built-in StatusRequestExtension would emit the legacy
			// TLS 1.2 OCSP body instead and change the wire bytes.
			return &tls.GenericExtension{Id: extStatusRequest, Data: nil}, nil
		}
		return &tls.StatusRequestExtension{}, nil
	case extSCT:
		return &tls.SCTExtension{}, nil
	case extPadding:
		return &tls.UtlsPaddingExtension{GetPaddingLen: tls.BoringPaddingStyle}, nil
	case extExtMasterSecret:
		return &tls.ExtendedMasterSecretExtension{}, nil
	case extSessionTicket:
		return &tls.SessionTicketExtension{}, nil
	case extECPointFormats:
		list, ok := u8Body(data)
		if !ok {
			return nil, fmt.Errorf("tlsfp: bad ec_point_formats body")
		}
		return &tls.SupportedPointsExtension{SupportedPoints: append([]uint8(nil), list...)}, nil
	case extRecordSizeLimit:
		// The extension data is a bare uint16 (no list wrapper).
		if len(data) != 2 {
			return nil, fmt.Errorf("tlsfp: bad record_size_limit body")
		}
		return &tls.FakeRecordSizeLimitExtension{Limit: be16(data)}, nil
	case extDelegatedCredentials:
		algs, err := buildSigAlgs(data)
		if err != nil {
			return nil, err
		}
		return &tls.FakeDelegatedCredentialsExtension{SupportedSignatureAlgorithms: algs}, nil
	case extRenegotiationInfo:
		return &tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient}, nil
	case extApplicationSettings:
		protos, err := alpnProtocols(data)
		if err != nil {
			return nil, err
		}
		return &tls.ApplicationSettingsExtension{SupportedProtocols: protos}, nil
	case extApplicationSettingsNew:
		protos, err := alpnProtocols(data)
		if err != nil {
			return nil, err
		}
		return &tls.ApplicationSettingsExtensionNew{SupportedProtocols: protos}, nil
	case extECH:
		return &tls.GREASEEncryptedClientHelloExtension{}, nil
	case extPSK:
		return nil, fmt.Errorf("tlsfp: capture contains pre_shared_key (session resumption) — capture from a fresh browser profile instead; replaying PSK identities would be a replay signal")
	default:
		// Unknown extension: replay the captured body verbatim. This is
		// byte-exact by construction and future-proof (a new browser
		// extension keeps the fingerprint identical until uTLS grows a
		// typed writer for it); per-connection state never lives in
		// unknown extensions — PSK above is the one hard error.
		return &tls.GenericExtension{Id: id, Data: append([]byte(nil), data...)}, nil
	}
}

// buildCurves parses supported_groups and maps GREASE onto the placeholder.
func buildCurves(data []byte) ([]tls.CurveID, error) {
	body, ok := u16Body(data)
	if !ok || len(body)%2 != 0 {
		return nil, fmt.Errorf("tlsfp: bad supported_groups body")
	}
	curves := make([]tls.CurveID, 0, len(body)/2)
	for len(body) >= 2 {
		curves = append(curves, tls.CurveID(greased(be16(body))))
		body = body[2:]
	}
	return curves, nil
}

// buildSigAlgs parses a 2-byte-prefixed signature-scheme list
// (signature_algorithms and delegated_credentials share the shape).
func buildSigAlgs(data []byte) ([]tls.SignatureScheme, error) {
	body, ok := u16Body(data)
	if !ok || len(body)%2 != 0 {
		return nil, fmt.Errorf("tlsfp: bad signature algorithm list body")
	}
	algs := make([]tls.SignatureScheme, 0, len(body)/2)
	for len(body) >= 2 {
		// Chrome leads the list with a GREASE scheme; the placeholder
		// keeps it a per-connection random value like the browser's.
		algs = append(algs, tls.SignatureScheme(greased(be16(body))))
		body = body[2:]
	}
	return algs, nil
}

// buildCertCompAlgs parses the RFC 8879 algorithm list (1-byte length).
func buildCertCompAlgs(data []byte) ([]tls.CertCompressionAlgo, error) {
	list, ok := u8Body(data)
	if !ok || len(list)%2 != 0 {
		return nil, fmt.Errorf("tlsfp: bad compress_certificate body")
	}
	algs := make([]tls.CertCompressionAlgo, 0, len(list)/2)
	for len(list) >= 2 {
		algs = append(algs, tls.CertCompressionAlgo(be16(list)))
		list = list[2:]
	}
	return algs, nil
}

// buildVersions parses supported_versions (1-byte length prefix).
func buildVersions(data []byte) ([]uint16, error) {
	list, ok := u8Body(data)
	if !ok || len(list)%2 != 0 {
		return nil, fmt.Errorf("tlsfp: bad supported_versions body")
	}
	vers := make([]uint16, 0, len(list)/2)
	for len(list) >= 2 {
		vers = append(vers, greased(be16(list)))
		list = list[2:]
	}
	return vers, nil
}

// buildKeyShares parses the key-share entries. GREASE entries keep a
// filler byte (uTLS re-GREASEs them); real groups get nil data so uTLS
// generates a fresh key pair per connection — replaying captured
// ephemeral keys would itself be a replay signal.
func buildKeyShares(data []byte) ([]tls.KeyShare, error) {
	list, ok := u16Body(data)
	if !ok {
		return nil, fmt.Errorf("tlsfp: bad key_share body")
	}
	var shares []tls.KeyShare
	for len(list) >= 4 {
		g := greased(be16(list))
		l := int(be16(list[2:]))
		if len(list) < 4+l {
			return nil, fmt.Errorf("tlsfp: bad key_share entry")
		}
		ks := tls.KeyShare{Group: tls.CurveID(g)}
		if g == tls.GREASE_PLACEHOLDER {
			ks.Data = []byte{0}
		}
		shares = append(shares, ks)
		list = list[4+l:]
	}
	return shares, nil
}

// zeroStatusRequest was replaced by tls.GenericExtension{Id: 0x0005}:
// the uTLS TLSExtension interface carries an unexported method and
// cannot be implemented outside the library.

// iterateExtensions walks the extensions of a ClientHello record and
// calls fn for every extension body. The walk is the single parser used
// both for the JSON model and the replay spec.
func iterateExtensions(raw []byte, fn func(id uint16, data []byte) error) error {
	if len(raw) < 6 || raw[0] != 0x16 {
		return errNotClientHello
	}
	recLen := int(be16(raw[3:]))
	if len(raw) < 5+recLen || recLen < 4 || raw[5] != 0x01 {
		return errNotClientHello
	}
	body := raw[9 : 5+recLen]
	if len(body) < 2+32+1 {
		return errNotClientHello
	}
	body = body[2:][32:] // legacy version + random
	sidLen := int(body[0])
	rest, ok := cut(body[1:], sidLen)
	if !ok {
		return errNotClientHello
	}
	csLen := int(be16(rest))
	if len(rest) < 2+csLen+1 {
		return errNotClientHello
	}
	compLen := int(rest[2+csLen])
	extBlock := rest[3+csLen+compLen:]
	if len(extBlock) == 0 {
		return nil
	}
	if len(extBlock) < 2 {
		return errNotClientHello
	}
	extLen := int(be16(extBlock))
	exts, ok := take(extBlock[2:], extLen)
	if !ok {
		return errNotClientHello
	}
	for len(exts) >= 4 {
		id := be16(exts)
		l := int(be16(exts[2:]))
		data, dok := take(exts[4:], l)
		if !dok {
			return errNotClientHello
		}
		if err := fn(id, data); err != nil {
			return err
		}
		exts = exts[4+l:]
	}
	return nil
}

// alpnProtocols parses an ALPN/ALPS protocol list: 2-byte list length,
// then 1-byte-length-prefixed names.
func alpnProtocols(data []byte) ([]string, error) {
	body, ok := u16Body(data)
	if !ok {
		return nil, fmt.Errorf("tlsfp: bad protocol list body")
	}
	var protos []string
	for len(body) > 0 {
		l := int(body[0])
		if len(body) < 1+l {
			return nil, fmt.Errorf("tlsfp: bad protocol list entry")
		}
		protos = append(protos, string(body[1:1+l]))
		body = body[1+l:]
	}
	return protos, nil
}

// greased maps a wire GREASE value onto the uTLS GREASE placeholder so
// uTLS re-randomizes it per connection; non-GREASE values pass through.
func greased(v uint16) uint16 {
	if isGREASE16(v) {
		return tls.GREASE_PLACEHOLDER
	}
	return v
}
