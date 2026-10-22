// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	// emptySHA256 is the hex SHA-256 of the empty payload.
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	amzDateFmt  = "20060102T150405Z"
)

// sigV4Params carries everything the AWS Signature Version 4 algorithm needs.
type sigV4Params struct {
	Method      string
	URI         string            // canonical (already URI-encoded) path
	Query       string            // canonical query string
	Headers     map[string]string // header name (any case) to value; all are signed
	PayloadHash string            // hex SHA-256 of the payload
	Time        time.Time
	Region      string
	Service     string
	AccessKey   string
	SecretKey   string
}

// uriEncode encodes s per the SigV4 rules: only A-Z a-z 0-9 - _ . ~ stay
// literal, everything else becomes %XX (upper-case hex). Slashes are kept
// when keepSlash is set (paths) and encoded otherwise (query components).
func uriEncode(s string, keepSlash bool) string {
	const hexUpper = "0123456789ABCDEF"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			sb.WriteByte(c)
		case c == '/' && keepSlash:
			sb.WriteByte(c)
		default:
			sb.WriteByte('%')
			sb.WriteByte(hexUpper[c>>4])
			sb.WriteByte(hexUpper[c&15])
		}
	}
	return sb.String()
}

// canonicalQuery builds the sorted, encoded query string from key/value pairs.
func canonicalQuery(kv map[string]string) string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, uriEncode(k, false)+"="+uriEncode(kv[k], false))
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// sign computes the canonical request, string to sign, signature and the
// Authorization header value.
func (p sigV4Params) sign() (canonicalRequest, stringToSign, signature, authorization string) {
	names := make([]string, 0, len(p.Headers))
	lower := make(map[string]string, len(p.Headers))
	for k, v := range p.Headers {
		l := strings.ToLower(k)
		names = append(names, l)
		lower[l] = strings.Join(strings.Fields(v), " ")
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + lower[n] + "\n")
	}
	signed := strings.Join(names, ";")
	canonicalRequest = strings.Join([]string{p.Method, p.URI, p.Query, ch.String(), signed, p.PayloadHash}, "\n")
	t := p.Time.UTC()
	date := t.Format("20060102")
	scope := date + "/" + p.Region + "/" + p.Service + "/aws4_request"
	stringToSign = sigV4Algorithm + "\n" + t.Format(amzDateFmt) + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))
	k := hmacSHA256([]byte("AWS4"+p.SecretKey), date)
	k = hmacSHA256(k, p.Region)
	k = hmacSHA256(k, p.Service)
	k = hmacSHA256(k, "aws4_request")
	signature = hex.EncodeToString(hmacSHA256(k, stringToSign))
	authorization = sigV4Algorithm + " Credential=" + p.AccessKey + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + signature
	return
}

// signRequest adds x-amz-date, x-amz-content-sha256, optional session token
// and the Authorization header to req. The request path must already be
// encoded with uriEncode (see S3Backend.newRequest).
func signRequest(req *http.Request, payloadHash string, now time.Time, region, access, secret, token string) {
	req.Header.Set("X-Amz-Date", now.UTC().Format(amzDateFmt))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if token != "" {
		req.Header.Set("X-Amz-Security-Token", token)
	}
	hdrs := map[string]string{"host": req.URL.Host}
	for _, h := range []string{"X-Amz-Date", "X-Amz-Content-Sha256", "X-Amz-Security-Token", "Content-Type"} {
		if v := req.Header.Get(h); v != "" {
			hdrs[h] = v
		}
	}
	p := sigV4Params{
		Method: req.Method, URI: req.URL.EscapedPath(), Query: req.URL.RawQuery,
		Headers: hdrs, PayloadHash: payloadHash, Time: now,
		Region: region, Service: "s3", AccessKey: access, SecretKey: secret,
	}
	_, _, _, auth := p.sign()
	req.Header.Set("Authorization", auth)
}
