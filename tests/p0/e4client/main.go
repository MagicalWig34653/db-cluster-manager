// SPDX-License-Identifier: AGPL-3.0-only

// Command e4client is a lab-only API client for experiment E4. It drives
// the real manager HTTP API like a browser would: it keeps cookies, sends
// the configured Origin on every request and answers passkey ceremonies
// with an ES256 software authenticator ("none" attestation), so the
// manager's WebAuthn verification runs unmodified. No browser is involved.
//
// State (authenticator key, credential, cookies) is kept in a 0600 JSON
// file between invocations. It holds a session credential: lab use only.
//
//	e4client -state F -base http://127.0.0.1:18480 -origin http://localhost:18480 setup -token-file T -account admin
//	e4client -state F ... login
//	e4client -state F ... req METHOD PATH [JSON-BODY]
//	e4client -state F keygen OUT   (synthetic backup recovery key)
//
// req prints "HTTP <status>" and the body; the exit status is 0 for 2xx and
// 3 otherwise.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

type state struct {
	Key        string            `json:"key"` // PKCS#8, base64
	CredID     string            `json:"credId"`
	UserHandle string            `json:"userHandle"`
	Counter    uint32            `json:"counter"`
	Cookies    map[string]string `json:"cookies"`
}

type client struct {
	base, origin, path string
	st                 state
	key                *ecdsa.PrivateKey
	http               *http.Client
}

func main() {
	fs := flag.NewFlagSet("e4client", flag.ExitOnError)
	statePath := fs.String("state", "", "state file (required)")
	base := fs.String("base", "http://127.0.0.1:18480", "manager base URL")
	origin := fs.String("origin", "http://localhost:18480", "Origin header (a configured passkey origin)")
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()
	if *statePath == "" || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: e4client -state F [-base URL] [-origin O] setup|login|req ...")
		os.Exit(2)
	}
	c, err := load(*statePath, *base, *origin)
	if err != nil {
		fail(err)
	}
	switch args[0] {
	case "setup":
		sf := flag.NewFlagSet("setup", flag.ExitOnError)
		tokenFile := sf.String("token-file", "", "file with the setup token")
		account := sf.String("account", "admin", "account ID")
		_ = sf.Parse(args[1:])
		err = c.setup(*tokenFile, *account)
	case "login":
		err = c.login()
	case "keygen":
		// keygen OUT: a synthetic recovery key for the lab backup stream,
		// written to OUT (0600); only the public key is printed.
		if len(args) != 2 {
			fail(errors.New("keygen OUT"))
		}
		k, kerr := backup.GenerateRecoveryKey()
		if kerr != nil {
			fail(kerr)
		}
		pub, kerr := k.StreamPublicKey()
		if kerr != nil {
			fail(kerr)
		}
		if kerr := os.WriteFile(args[1], []byte(k.String()+"\n"), 0o600); kerr != nil {
			fail(kerr)
		}
		fmt.Println(pub.String())
		return
	case "req":
		if len(args) < 3 {
			fail(errors.New("req METHOD PATH [BODY]"))
		}
		body := ""
		if len(args) > 3 {
			body = args[3]
		}
		var code int
		var out []byte
		code, out, err = c.do(args[1], args[2], body)
		if err == nil {
			fmt.Printf("HTTP %d\n%s\n", code, bytes.TrimSpace(out))
			if serr := c.save(); serr != nil {
				fail(serr)
			}
			if code < 200 || code > 299 {
				os.Exit(3)
			}
			return
		}
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		fail(err)
	}
	if err := c.save(); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "e4client:", err)
	os.Exit(1)
}

func load(path, base, origin string) (*client, error) {
	c := &client{base: strings.TrimRight(base, "/"), origin: origin, path: path,
		http: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return nil, err
		}
		c.key = key
		c.st = state{Key: base64.StdEncoding.EncodeToString(der), CredID: b64(id), Cookies: map[string]string{}}
		return c, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(raw, &c.st); err != nil {
		return nil, err
	}
	der, err := base64.StdEncoding.DecodeString(c.st.Key)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	var ok bool
	if c.key, ok = k.(*ecdsa.PrivateKey); !ok {
		return nil, errors.New("state key is not ECDSA")
	}
	if c.st.Cookies == nil {
		c.st.Cookies = map[string]string{}
	}
	return c, nil
}

func (c *client) save() error {
	raw, err := json.MarshalIndent(c.st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, raw, 0o600)
}

func (c *client) do(method, path, body string) (int, []byte, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Accept", "application/json")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.st.Cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, err
	}
	for _, ck := range resp.Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.st.Cookies, ck.Name)
		} else {
			c.st.Cookies[ck.Name] = ck.Value
		}
	}
	return resp.StatusCode, out, nil
}

type challenge struct {
	CeremonyID string          `json:"ceremonyId"`
	Options    json.RawMessage `json:"options"`
}

func (c *client) post(path string, in any, want int) ([]byte, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	code, out, err := c.do("POST", path, string(raw))
	if err != nil {
		return nil, err
	}
	if code != want {
		return nil, fmt.Errorf("POST %s: HTTP %d: %s", path, code, bytes.TrimSpace(out))
	}
	return out, nil
}

func (c *client) setup(tokenFile, account string) error {
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(tok))
	out, err := c.post("/api/v1/setup/begin", map[string]string{"setupToken": token, "accountId": account,
		"displayName": "E4 lab administrator", "passkeyLabel": "e4-software-key"}, http.StatusOK)
	if err != nil {
		return err
	}
	var ch challenge
	if err := json.Unmarshal(out, &ch); err != nil {
		return err
	}
	cred, err := c.create(ch.Options)
	if err != nil {
		return err
	}
	out, err = c.post("/api/v1/setup/finish", map[string]any{"setupToken": token, "ceremonyId": ch.CeremonyID,
		"credential": json.RawMessage(cred)}, http.StatusCreated)
	if err != nil {
		return err
	}
	var res struct {
		AccountID     string   `json:"accountId"`
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return err
	}
	// Recovery codes are a credential: only their number is printed.
	fmt.Printf("setup finished: account %s, %d recovery codes issued (not printed), session cookie set: %v\n",
		res.AccountID, len(res.RecoveryCodes), len(c.st.Cookies) > 0)
	return nil
}

func (c *client) login() error {
	out, err := c.post("/api/v1/auth/login/begin", map[string]any{}, http.StatusOK)
	if err != nil {
		return err
	}
	var ch challenge
	if err := json.Unmarshal(out, &ch); err != nil {
		return err
	}
	cred, err := c.get(ch.Options)
	if err != nil {
		return err
	}
	out, err = c.post("/api/v1/auth/login/finish", map[string]any{"ceremonyId": ch.CeremonyID, "credential": json.RawMessage(cred)}, http.StatusOK)
	if err != nil {
		return err
	}
	fmt.Printf("login finished: %s\n", bytes.TrimSpace(out))
	return nil
}

// ---- software authenticator (ES256, "none" attestation)

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

func (c *client) clientData(typ, challenge string) ([]byte, error) {
	return json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": c.origin, "crossOrigin": false})
}

func (c *client) authData(rpID string, flags byte, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	var b bytes.Buffer
	b.Write(h[:])
	b.WriteByte(flags)
	_ = binary.Write(&b, binary.BigEndian, c.st.Counter)
	b.Write(attested)
	return b.Bytes()
}

const (
	flagUP = 0x01
	flagUV = 0x04
	flagAT = 0x40
)

func (c *client) create(raw []byte) ([]byte, error) {
	var o wireOptions
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	c.st.UserHandle, c.st.Counter = o.User.ID, 0
	cdj, err := c.clientData("webauthn.create", o.Challenge)
	if err != nil {
		return nil, err
	}
	credID, err := base64.RawURLEncoding.DecodeString(c.st.CredID)
	if err != nil {
		return nil, err
	}
	pub, err := c.key.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	cose := cbor(cborMap{{1, 2}, {3, -7}, {-1, 1}, {-2, pub[1:33]}, {-3, pub[33:65]}})
	var attested bytes.Buffer
	attested.Write(make([]byte, 16))
	_ = binary.Write(&attested, binary.BigEndian, uint16(len(credID)))
	attested.Write(credID)
	attested.Write(cose)
	ad := c.authData(o.RP.ID, flagUP|flagUV|flagAT, attested.Bytes())
	att := cbor(cborMap{{"fmt", "none"}, {"attStmt", cborMap{}}, {"authData", ad}})
	return json.Marshal(map[string]any{
		"id": c.st.CredID, "rawId": c.st.CredID, "type": "public-key",
		"response":                map[string]any{"clientDataJSON": b64(cdj), "attestationObject": b64(att), "transports": []string{"internal"}},
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
	})
}

func (c *client) get(raw []byte) ([]byte, error) {
	var o wireOptions
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	c.st.Counter++
	cdj, err := c.clientData("webauthn.get", o.Challenge)
	if err != nil {
		return nil, err
	}
	ad := c.authData(o.RPID, flagUP|flagUV, nil)
	cdh := sha256.Sum256(cdj)
	d := sha256.Sum256(append(bytes.Clone(ad), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, c.key, d[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": c.st.CredID, "rawId": c.st.CredID, "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64(cdj), "authenticatorData": b64(ad), "signature": b64(sig),
			"userHandle": c.st.UserHandle},
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
	})
}

// ---- minimal CBOR encoder (maps, ints, byte and text strings)

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
