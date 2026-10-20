// SPDX-License-Identifier: AGPL-3.0-only

package passkey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

// This file is a software WebAuthn authenticator plus client for tests. It
// produces real ES256 signatures, authenticatorData, clientDataJSON and
// CBOR attestation objects ("none" and "packed" self attestation), so the
// library's verification runs unmodified.

// --- minimal CBOR encoder (definite lengths only) ---

type cborKV struct{ k, v any }
type cborMap []cborKV

func cborHead(b *bytes.Buffer, major byte, n uint64) {
	m := major << 5
	switch {
	case n < 24:
		b.WriteByte(m | byte(n))
	case n < 1<<8:
		b.WriteByte(m | 24)
		b.WriteByte(byte(n))
	case n < 1<<16:
		b.WriteByte(m | 25)
		_ = binary.Write(b, binary.BigEndian, uint16(n))
	default:
		b.WriteByte(m | 26)
		_ = binary.Write(b, binary.BigEndian, uint32(n))
	}
}

func cborEncode(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case int:
		if x >= 0 {
			cborHead(b, 0, uint64(x))
		} else {
			cborHead(b, 1, uint64(-1-x))
		}
	case []byte:
		cborHead(b, 2, uint64(len(x)))
		b.Write(x)
	case string:
		cborHead(b, 3, uint64(len(x)))
		b.WriteString(x)
	case cborMap:
		cborHead(b, 5, uint64(len(x)))
		for _, kv := range x {
			cborEncode(b, kv.k)
			cborEncode(b, kv.v)
		}
	default:
		panic("cbor: unsupported type")
	}
}

func cbor(v any) []byte {
	var b bytes.Buffer
	cborEncode(&b, v)
	return b.Bytes()
}

// --- authenticator ---

const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

type softAuthenticator struct {
	t          *testing.T
	key        *ecdsa.PrivateKey
	credID     []byte
	userHandle []byte
	counter    uint32
	// countless authenticators always report 0 (synced passkeys).
	countless bool
	// synced sets the backup-eligible and backup-state flags.
	synced bool
}

func newSoftAuthenticator(t *testing.T) *softAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &softAuthenticator{t: t, key: key, credID: id}
}

// clone returns an authenticator holding the same private key and counter,
// as a cloned hardware key would.
func (a *softAuthenticator) clone() *softAuthenticator {
	c := *a
	c.credID = bytes.Clone(a.credID)
	c.userHandle = bytes.Clone(a.userHandle)
	return &c
}

// tweak alters one ceremony. Zero values mean "behave correctly".
type tweak struct {
	origin     string // clientDataJSON origin; default the test origin
	rpID       string // RP ID hashed into authenticatorData; default from options
	challenge  string // challenge in clientDataJSON; default from options
	clientType string // clientDataJSON type; default webauthn.create/get
	noUV       bool   // clear the user-verified flag
	badSig     bool   // flip a bit in the signature (assertion or packed attStmt)
	format     string // "none" (default) or "packed"
	userHandle []byte // assertion userHandle override
	noHandle   bool   // omit userHandle from the assertion
}

type wireOptions struct {
	Challenge string `json:"challenge"`
	RP        struct {
		ID string `json:"id"`
	} `json:"rp"`
	RPID string `json:"rpId"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	AuthenticatorSelection struct {
		UserVerification string `json:"userVerification"`
		ResidentKey      string `json:"residentKey"`
	} `json:"authenticatorSelection"`
	UserVerification string `json:"userVerification"`
	Attestation      string `json:"attestation"`
	ExcludeCreds     []struct {
		ID string `json:"id"`
	} `json:"excludeCredentials"`
	AllowCreds []struct {
		ID string `json:"id"`
	} `json:"allowCredentials"`
}

func parseOptions(t *testing.T, raw []byte) wireOptions {
	t.Helper()
	var o wireOptions
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("options: %v", err)
	}
	return o
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 %q: %v", s, err)
	}
	return b
}

func (a *softAuthenticator) clientData(typ, challenge, origin string) []byte {
	b, err := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		a.t.Fatal(err)
	}
	return b
}

func (a *softAuthenticator) flags(base byte, tw tweak) byte {
	f := base | flagUP | flagUV
	if tw.noUV {
		f &^= flagUV
	}
	if a.synced {
		f |= flagBE | flagBS
	}
	return f
}

func (a *softAuthenticator) authData(rpID string, flags byte, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	var b bytes.Buffer
	b.Write(h[:])
	b.WriteByte(flags)
	_ = binary.Write(&b, binary.BigEndian, a.counter)
	b.Write(attested)
	return b.Bytes()
}

func (a *softAuthenticator) sign(msg []byte, corrupt bool) []byte {
	d := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, d[:])
	if err != nil {
		a.t.Fatal(err)
	}
	if corrupt {
		sig[len(sig)-1] ^= 0x01
	}
	return sig
}

func (a *softAuthenticator) coseKey() []byte {
	pub, err := a.key.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		a.t.Fatal(err)
	}
	return cbor(cborMap{
		{1, 2},  // kty: EC2
		{3, -7}, // alg: ES256
		{-1, 1}, // crv: P-256
		{-2, pub[1:33]},
		{-3, pub[33:65]},
	})
}

// create answers navigator.credentials.create for the given options JSON
// and returns the PublicKeyCredential JSON.
func (a *softAuthenticator) create(options []byte, origin string, tw tweak) []byte {
	o := parseOptions(a.t, options)
	a.userHandle = mustB64(a.t, o.User.ID)
	rpID := o.RP.ID
	if tw.rpID != "" {
		rpID = tw.rpID
	}
	if tw.origin != "" {
		origin = tw.origin
	}
	challenge := o.Challenge
	if tw.challenge != "" {
		challenge = tw.challenge
	}
	typ := "webauthn.create"
	if tw.clientType != "" {
		typ = tw.clientType
	}
	if !a.countless {
		a.counter = 0
	}
	cdj := a.clientData(typ, challenge, origin)

	var attested bytes.Buffer
	attested.Write(make([]byte, 16)) // AAGUID: zero
	_ = binary.Write(&attested, binary.BigEndian, uint16(len(a.credID)))
	attested.Write(a.credID)
	attested.Write(a.coseKey())
	ad := a.authData(rpID, a.flags(flagAT, tw), attested.Bytes())

	format := tw.format
	if format == "" {
		format = "none"
	}
	stmt := cborMap{}
	if format == "packed" {
		cdh := sha256.Sum256(cdj)
		stmt = cborMap{{"alg", -7}, {"sig", a.sign(append(bytes.Clone(ad), cdh[:]...), tw.badSig)}}
	}
	att := cbor(cborMap{{"fmt", format}, {"attStmt", stmt}, {"authData", ad}})
	out, err := json.Marshal(map[string]any{
		"id":    b64(a.credID),
		"rawId": b64(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64(cdj),
			"attestationObject": b64(att),
			"transports":        []string{"internal"},
		},
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{},
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return out
}

// get answers navigator.credentials.get and returns the assertion JSON.
func (a *softAuthenticator) get(options []byte, origin string, tw tweak) []byte {
	o := parseOptions(a.t, options)
	rpID := o.RPID
	if tw.rpID != "" {
		rpID = tw.rpID
	}
	if tw.origin != "" {
		origin = tw.origin
	}
	challenge := o.Challenge
	if tw.challenge != "" {
		challenge = tw.challenge
	}
	typ := "webauthn.get"
	if tw.clientType != "" {
		typ = tw.clientType
	}
	if !a.countless {
		a.counter++
	}
	cdj := a.clientData(typ, challenge, origin)
	ad := a.authData(rpID, a.flags(0, tw), nil)
	cdh := sha256.Sum256(cdj)
	sig := a.sign(append(bytes.Clone(ad), cdh[:]...), tw.badSig)
	resp := map[string]any{
		"clientDataJSON":    b64(cdj),
		"authenticatorData": b64(ad),
		"signature":         b64(sig),
	}
	handle := a.userHandle
	if tw.userHandle != nil {
		handle = tw.userHandle
	}
	if !tw.noHandle {
		resp["userHandle"] = b64(handle)
	}
	out, err := json.Marshal(map[string]any{
		"id":                      b64(a.credID),
		"rawId":                   b64(a.credID),
		"type":                    "public-key",
		"response":                resp,
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{},
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return out
}
