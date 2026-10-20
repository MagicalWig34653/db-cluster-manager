// SPDX-License-Identifier: AGPL-3.0-only

package api

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

// A minimal software WebAuthn authenticator and client (ES256, "none"
// attestation), trimmed from internal/passkey/authenticator_test.go. It
// produces real signatures, so the library verification runs unmodified;
// no browser is involved.

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

const (
	flagUP = 0x01
	flagUV = 0x04
	flagAT = 0x40
)

type softAuthenticator struct {
	t          *testing.T
	key        *ecdsa.PrivateKey
	credID     []byte
	userHandle []byte
	counter    uint32
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

type wireOptions struct {
	Challenge string `json:"challenge"`
	RP        struct {
		ID string `json:"id"`
	} `json:"rp"`
	RPID string `json:"rpId"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (a *softAuthenticator) options(raw []byte) wireOptions {
	var o wireOptions
	if err := json.Unmarshal(raw, &o); err != nil {
		a.t.Fatalf("options: %v", err)
	}
	return o
}

func (a *softAuthenticator) clientData(typ, challenge, origin string) []byte {
	b, err := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		a.t.Fatal(err)
	}
	return b
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

func (a *softAuthenticator) coseKey() []byte {
	pub, err := a.key.PublicKey.Bytes()
	if err != nil {
		a.t.Fatal(err)
	}
	return cbor(cborMap{{1, 2}, {3, -7}, {-1, 1}, {-2, pub[1:33]}, {-3, pub[33:65]}})
}

// create answers navigator.credentials.create.
func (a *softAuthenticator) create(options []byte, origin string) []byte {
	o := a.options(options)
	h, err := base64.RawURLEncoding.DecodeString(o.User.ID)
	if err != nil {
		a.t.Fatal(err)
	}
	a.userHandle = h
	a.counter = 0
	cdj := a.clientData("webauthn.create", o.Challenge, origin)
	var attested bytes.Buffer
	attested.Write(make([]byte, 16))
	_ = binary.Write(&attested, binary.BigEndian, uint16(len(a.credID)))
	attested.Write(a.credID)
	attested.Write(a.coseKey())
	ad := a.authData(o.RP.ID, flagUP|flagUV|flagAT, attested.Bytes())
	att := cbor(cborMap{{"fmt", "none"}, {"attStmt", cborMap{}}, {"authData", ad}})
	out, err := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64(cdj), "attestationObject": b64(att), "transports": []string{"internal"},
		},
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return out
}

// get answers navigator.credentials.get.
func (a *softAuthenticator) get(options []byte, origin string) []byte {
	o := a.options(options)
	a.counter++
	cdj := a.clientData("webauthn.get", o.Challenge, origin)
	ad := a.authData(o.RPID, flagUP|flagUV, nil)
	cdh := sha256.Sum256(cdj)
	d := sha256.Sum256(append(bytes.Clone(ad), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, d[:])
	if err != nil {
		a.t.Fatal(err)
	}
	out, err := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64(cdj), "authenticatorData": b64(ad), "signature": b64(sig),
			"userHandle": b64(a.userHandle),
		},
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return out
}
