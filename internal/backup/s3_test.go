// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- SigV4 vectors ----

func mustTime(t *testing.T, layout, v string) time.Time {
	t.Helper()
	tm, err := time.Parse(layout, v)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// TestSigV4GetVanilla uses the "get-vanilla" vector of the AWS Signature
// Version 4 test suite (aws4_testsuite).
func TestSigV4GetVanilla(t *testing.T) {
	p := sigV4Params{
		Method: "GET", URI: "/", Query: "",
		Headers:     map[string]string{"Host": "example.amazonaws.com", "X-Amz-Date": "20150830T123600Z"},
		PayloadHash: emptySHA256,
		Time:        mustTime(t, amzDateFmt, "20150830T123600Z"),
		Region:      "us-east-1", Service: "service",
		AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	cr, sts, sig, auth := p.sign()
	wantCR := "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptySHA256
	if cr != wantCR {
		t.Fatalf("canonical request:\n%s", cr)
	}
	wantSTS := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\nbb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63"
	if sts != wantSTS {
		t.Fatalf("string to sign:\n%s", sts)
	}
	if sig != "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31" {
		t.Fatalf("signature %s", sig)
	}
	if auth != "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature="+sig {
		t.Fatalf("auth %s", auth)
	}
}

// TestSigV4S3DocExamples uses the worked examples of the Amazon S3
// documentation ("Signature Calculations for the Authorization Header").
func TestSigV4S3DocExamples(t *testing.T) {
	const ak, sk = "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	tm := mustTime(t, amzDateFmt, "20130524T000000Z")
	base := func(m, uri, q, host, hash string, extra map[string]string) sigV4Params {
		h := map[string]string{"host": host, "x-amz-content-sha256": hash, "x-amz-date": "20130524T000000Z"}
		for k, v := range extra {
			h[k] = v
		}
		return sigV4Params{Method: m, URI: uri, Query: q, Headers: h, PayloadHash: hash, Time: tm,
			Region: "us-east-1", Service: "s3", AccessKey: ak, SecretKey: sk}
	}
	tests := []struct {
		name string
		p    sigV4Params
		sig  string
	}{
		{"get object with range", base("GET", "/test.txt", "", "examplebucket.s3.amazonaws.com", emptySHA256, map[string]string{"range": "bytes=0-9"}),
			"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		{"put object", base("PUT", "/test%24file.text", "", "examplebucket.s3.amazonaws.com", sha256Hex([]byte("Welcome to Amazon S3.")),
			map[string]string{"date": "Fri, 24 May 2013 00:00:00 GMT", "x-amz-storage-class": "REDUCED_REDUNDANCY"}),
			"98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"},
		{"list objects", base("GET", "/", canonicalQuery(map[string]string{"max-keys": "2", "prefix": "J"}), "examplebucket.s3.amazonaws.com", emptySHA256, nil),
			"34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	}
	for _, tc := range tests {
		if _, _, sig, _ := tc.p.sign(); sig != tc.sig {
			t.Errorf("%s: signature %s, want %s", tc.name, sig, tc.sig)
		}
	}
}

// TestSigV4StepByStep recomputes a signature for a request with an
// awkward key and query using plain stdlib calls, written out step by step
// and sharing no code with the implementation.
func TestSigV4StepByStep(t *testing.T) {
	tm := time.Date(2026, 10, 20, 8, 30, 15, 0, time.UTC)
	query := map[string]string{"prefix": "a b/c+d", "list-type": "2"}
	p := sigV4Params{
		Method: "GET", URI: uriEncode("/bkt/dir/ü file~1.txt", true), Query: canonicalQuery(query),
		Headers:     map[string]string{"Host": "s3.example.net:9000", "X-Amz-Date": "20261020T083015Z", "X-Amz-Content-Sha256": emptySHA256},
		PayloadHash: emptySHA256, Time: tm, Region: "eu-central-1", Service: "s3", AccessKey: "AK", SecretKey: "s3cr3t",
	}
	_, _, got, _ := p.sign()

	// Independent computation.
	if p.URI != "/bkt/dir/%C3%BC%20file~1.txt" {
		t.Fatalf("uri %s", p.URI)
	}
	if p.Query != "list-type=2&prefix=a%20b%2Fc%2Bd" {
		t.Fatalf("query %s", p.Query)
	}
	creq := "GET\n/bkt/dir/%C3%BC%20file~1.txt\nlist-type=2&prefix=a%20b%2Fc%2Bd\n" +
		"host:s3.example.net:9000\nx-amz-content-sha256:" + emptySHA256 + "\nx-amz-date:20261020T083015Z\n\n" +
		"host;x-amz-content-sha256;x-amz-date\n" + emptySHA256
	h := sha256.Sum256([]byte(creq))
	sts := "AWS4-HMAC-SHA256\n20261020T083015Z\n20261020/eu-central-1/s3/aws4_request\n" + hex.EncodeToString(h[:])
	mac := func(k []byte, s string) []byte { m := hmac.New(sha256.New, k); m.Write([]byte(s)); return m.Sum(nil) }
	k := mac(mac(mac(mac([]byte("AWS4s3cr3t"), "20261020"), "eu-central-1"), "s3"), "aws4_request")
	want := hex.EncodeToString(mac(k, sts))
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

// ---- fake S3 server ----

type fakeS3 struct {
	t        *testing.T
	bucket   string
	host     string
	region   string
	ak, sk   string
	virtual  bool
	pageSize int

	mu      sync.Mutex
	objs    map[string][]byte
	mods    map[string]time.Time
	uploads map[string]map[int][]byte
	nextID  int

	failNext   atomic.Int32 // respond 503 to the next n requests
	failStatus atomic.Int32
	requests   atomic.Int32
	multiPuts  atomic.Int32
	aborts     atomic.Int32
	listCalls  atomic.Int32
	sigErrors  atomic.Int32
	onPart     func(n int)
}

func newFakeS3(t *testing.T) *fakeS3 {
	return &fakeS3{t: t, bucket: "bkt", region: "eu-west-1", ak: "AKTEST", sk: "SKTEST/secret", pageSize: 1000,
		objs: map[string][]byte{}, mods: map[string]time.Time{}, uploads: map[string]map[int][]byte{}}
}

// refEncode is the fake's independent URI encoder.
func refEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	return strings.ReplaceAll(e, "%7E", "~")
}

// verify checks the SigV4 signature of r with the fake's own implementation
// and returns the request body.
func (f *fakeS3) verify(r *http.Request) ([]byte, error) {
	auth := r.Header.Get("Authorization")
	const pre = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(auth, pre) {
		return nil, errors.New("no sigv4 authorization")
	}
	fields := map[string]string{}
	for _, part := range strings.Split(auth[len(pre):], ", ") {
		k, v, _ := strings.Cut(part, "=")
		fields[k] = v
	}
	cred := strings.Split(fields["Credential"], "/")
	if len(cred) != 5 || cred[0] != f.ak || cred[2] != f.region || cred[3] != "s3" || cred[4] != "aws4_request" {
		return nil, fmt.Errorf("bad credential scope %q", fields["Credential"])
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	bh := sha256.Sum256(body)
	if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(bh[:]) {
		return nil, errors.New("x-amz-content-sha256 does not match body")
	}
	date := r.Header.Get("X-Amz-Date")
	if !strings.HasPrefix(date, cred[1]) {
		return nil, errors.New("date/scope mismatch")
	}
	at, err := time.Parse(amzDateFmt, date)
	if err != nil || time.Since(at) > 5*time.Minute || time.Until(at) > 5*time.Minute {
		return nil, errors.New("request time skew")
	}
	rawPath, rawQuery, _ := strings.Cut(r.RequestURI, "?")
	// Canonical query: decode and re-encode, sorted.
	var qs []string
	if rawQuery != "" {
		for _, pair := range strings.Split(rawQuery, "&") {
			k, v, _ := strings.Cut(pair, "=")
			dk, _ := url.PathUnescape(k)
			dv, _ := url.PathUnescape(v)
			qs = append(qs, refEncode(dk)+"="+refEncode(dv))
		}
		sort.Strings(qs)
	}
	signed := strings.Split(fields["SignedHeaders"], ";")
	var ch strings.Builder
	for _, h := range signed {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		ch.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	for _, need := range []string{"host", "x-amz-date", "x-amz-content-sha256"} {
		found := false
		for _, h := range signed {
			found = found || h == need
		}
		if !found {
			return nil, fmt.Errorf("header %s not signed", need)
		}
	}
	creq := strings.Join([]string{r.Method, rawPath, strings.Join(qs, "&"), ch.String(), fields["SignedHeaders"], r.Header.Get("X-Amz-Content-Sha256")}, "\n")
	ch2 := sha256.Sum256([]byte(creq))
	sts := "AWS4-HMAC-SHA256\n" + date + "\n" + strings.Join(cred[1:], "/") + "\n" + hex.EncodeToString(ch2[:])
	mac := func(k []byte, s string) []byte { m := hmac.New(sha256.New, k); m.Write([]byte(s)); return m.Sum(nil) }
	k := mac(mac(mac(mac([]byte("AWS4"+f.sk), cred[1]), cred[2]), cred[3]), cred[4])
	if hex.EncodeToString(mac(k, sts)) != fields["Signature"] {
		return nil, errors.New("SignatureDoesNotMatch")
	}
	return body, nil
}

func s3err(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, msg)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if n := f.failNext.Load(); n > 0 && f.failNext.CompareAndSwap(n, n-1) {
		st := int(f.failStatus.Load())
		if st == 0 {
			st = 503
		}
		s3err(w, st, "SlowDown", "try later")
		return
	}
	body, err := f.verify(r)
	if err != nil {
		f.sigErrors.Add(1)
		s3err(w, 403, "SignatureDoesNotMatch", err.Error())
		return
	}
	path := r.URL.Path
	if f.virtual {
		if r.Host != f.bucket+"."+f.hostOnly() {
			s3err(w, 400, "InvalidRequest", "bad virtual host "+r.Host)
			return
		}
	} else {
		pre := "/" + f.bucket
		if path != pre && !strings.HasPrefix(path, pre+"/") {
			s3err(w, 404, "NoSuchBucket", "no bucket")
			return
		}
		path = strings.TrimPrefix(path, pre)
	}
	key := strings.TrimPrefix(path, "/")
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case key == "" && r.Method == "GET" && q.Get("list-type") == "2":
		f.listCalls.Add(1)
		f.list(w, q)
	case r.Method == "PUT" && q.Get("partNumber") != "":
		n, _ := strconv.Atoi(q.Get("partNumber"))
		up, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			s3err(w, 404, "NoSuchUpload", "x")
			return
		}
		if f.onPart != nil {
			f.mu.Unlock()
			f.onPart(n)
			f.mu.Lock()
		}
		up[n] = body
		w.Header().Set("ETag", fmt.Sprintf("\"etag-%d\"", n))
	case r.Method == "PUT":
		f.objs[key] = body
		f.mods[key] = time.Now().UTC()
	case r.Method == "POST" && q.Has("uploads"):
		f.nextID++
		id := fmt.Sprintf("up%d", f.nextID)
		f.uploads[id] = map[int][]byte{}
		f.multiPuts.Add(1)
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == "POST" && q.Get("uploadId") != "":
		up, ok := f.uploads[q.Get("uploadId")]
		if !ok {
			s3err(w, 404, "NoSuchUpload", "x")
			return
		}
		var c completeUpload
		if xml.Unmarshal(body, &c) != nil || len(c.Parts) == 0 {
			s3err(w, 400, "MalformedXML", "x")
			return
		}
		var all []byte
		for i, p := range c.Parts {
			if p.PartNumber != i+1 || p.ETag != fmt.Sprintf("\"etag-%d\"", p.PartNumber) || up[p.PartNumber] == nil {
				s3err(w, 400, "InvalidPart", "x")
				return
			}
			all = append(all, up[p.PartNumber]...)
		}
		f.objs[key], f.mods[key] = all, time.Now().UTC()
		delete(f.uploads, q.Get("uploadId"))
		fmt.Fprint(w, "<CompleteMultipartUploadResult><ETag>x</ETag></CompleteMultipartUploadResult>")
	case r.Method == "DELETE" && q.Get("uploadId") != "":
		f.aborts.Add(1)
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(204)
	case r.Method == "GET" || r.Method == "HEAD":
		d, ok := f.objs[key]
		if !ok {
			if r.Method == "HEAD" {
				w.WriteHeader(404)
				return
			}
			s3err(w, 404, "NoSuchKey", "nope")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(d)))
		if r.Method == "GET" {
			_, _ = w.Write(d)
		}
	case r.Method == "DELETE":
		delete(f.objs, key)
		w.WriteHeader(204)
	default:
		s3err(w, 400, "InvalidRequest", "unhandled "+r.Method+" "+r.URL.String())
	}
}

func (f *fakeS3) hostOnly() string { return f.host }

func (f *fakeS3) list(w http.ResponseWriter, q url.Values) {
	prefix := q.Get("prefix")
	var keys []string
	for k := range f.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if tok := q.Get("continuation-token"); tok != "" {
		i := sort.SearchStrings(keys, tok)
		start = i
	}
	end := min(start+f.pageSize, len(keys))
	var sb strings.Builder
	sb.WriteString("<ListBucketResult>")
	for _, k := range keys[start:end] {
		var ek bytes.Buffer
		_ = xml.EscapeText(&ek, []byte(k))
		fmt.Fprintf(&sb, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified></Contents>",
			ek.String(), len(f.objs[k]), f.mods[k].Format("2006-01-02T15:04:05.000Z"))
	}
	if end < len(keys) {
		fmt.Fprintf(&sb, "<IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken>", keys[end])
	} else {
		sb.WriteString("<IsTruncated>false</IsTruncated>")
	}
	sb.WriteString("</ListBucketResult>")
	_, _ = io.WriteString(w, sb.String())
}

type fakeS3Env struct {
	*fakeS3
	srv *httptest.Server
	be  *S3Backend
}

func startFakeS3(t *testing.T, virtual bool, mutate func(*S3Config)) *fakeS3Env {
	f := newFakeS3(t)
	f.virtual = virtual
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	f.host = u.Host
	cfg := S3Config{Endpoint: srv.URL, Region: f.region, Bucket: f.bucket, Prefix: "pre/", AccessKey: f.ak, SecretKey: f.sk,
		AddressingStyle: "path", RetryBase: time.Millisecond, MaxRetries: 3}
	if virtual {
		cfg.AddressingStyle = "virtual"
		addr := u.Host
		cfg.HTTPClient = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, addr)
		}}}
	}
	if mutate != nil {
		mutate(&cfg)
	}
	be, err := NewS3Backend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeS3Env{fakeS3: f, srv: srv, be: be}
}

func (f *fakeS3) stored(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objs[key]
}

// ---- tests ----

func TestS3SharedSuitePathStyle(t *testing.T) {
	e := startFakeS3(t, false, nil)
	testBackend(t, e.be)
	if e.sigErrors.Load() != 0 {
		t.Fatalf("%d signature failures", e.sigErrors.Load())
	}
	if len(e.stored("pre/mgr-2")) == 0 {
		t.Fatal("prefix not applied")
	}
}

func TestS3SharedSuiteVirtualHosted(t *testing.T) {
	e := startFakeS3(t, true, nil)
	testBackend(t, e.be)
	if e.sigErrors.Load() != 0 {
		t.Fatalf("%d signature failures", e.sigErrors.Load())
	}
}

func TestS3ListPaginationAndPrefixScope(t *testing.T) {
	e := startFakeS3(t, false, nil)
	e.pageSize = 3
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		if err := e.be.Put(ctx, fmt.Sprintf("snap-%02d", i), strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	e.objs["other/foreign"] = []byte("y")  // outside the configured prefix
	e.objs["pre/sub/nested"] = []byte("y") // nested key is not one of our flat objects
	e.mu.Unlock()
	l, err := e.be.List(ctx, "snap-")
	if err != nil || len(l) != 8 || l[0].Name != "snap-00" || l[7].Name != "snap-07" || l[0].Size != 1 || l[0].ModTime.IsZero() {
		t.Fatalf("%v %+v", err, l)
	}
	if e.listCalls.Load() != 3 {
		t.Fatalf("list calls %d", e.listCalls.Load())
	}
	if l, _ = e.be.List(ctx, ""); len(l) != 8 {
		t.Fatalf("all: %+v", l)
	}
}

func TestS3MultipartBoundedAndExact(t *testing.T) {
	for _, size := range []int{10, 11, 20, 25, 3} { // part size 10: 25 -> 3 parts, 20 -> exact multiple
		e := startFakeS3(t, false, func(c *S3Config) { c.PartSize = 10 })
		data := bytes.Repeat([]byte("0123456789abcdefghijklmnopqrstuvwxyz"), 2)[:size]
		// A reader that returns tiny reads, to exercise buffering.
		if err := e.be.Put(context.Background(), "big", io.MultiReader(bytes.NewReader(data[:size/2]), bytes.NewReader(data[size/2:]))); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(e.stored("pre/big"), data) {
			t.Fatalf("size %d: content mismatch %q", size, e.stored("pre/big"))
		}
		wantMulti := int32(0)
		if size > 10 {
			wantMulti = 1
		}
		if e.multiPuts.Load() != wantMulti {
			t.Fatalf("size %d: multipart uploads %d", size, e.multiPuts.Load())
		}
		if e.sigErrors.Load() != 0 {
			t.Fatalf("size %d: signature errors", size)
		}
	}
}

func TestS3MultipartAbortOnReaderError(t *testing.T) {
	e := startFakeS3(t, false, func(c *S3Config) { c.PartSize = 10 })
	r := io.MultiReader(strings.NewReader("0123456789abcdefghij0123"), failReader{})
	if err := e.be.Put(context.Background(), "big", r); err == nil {
		t.Fatal("expected error")
	}
	if e.aborts.Load() != 1 || e.stored("pre/big") != nil || len(e.uploads) != 0 {
		t.Fatalf("aborts=%d", e.aborts.Load())
	}
}

func TestS3MultipartCancelMidUploadAborts(t *testing.T) {
	e := startFakeS3(t, false, func(c *S3Config) { c.PartSize = 10 })
	ctx, cancel := context.WithCancel(context.Background())
	e.onPart = func(n int) {
		if n == 2 {
			cancel()
		}
	}
	err := e.be.Put(ctx, "big", strings.NewReader(strings.Repeat("z", 100)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if e.aborts.Load() != 1 {
		t.Fatalf("aborts %d", e.aborts.Load())
	}
	if e.stored("pre/big") != nil {
		t.Fatal("partial object visible")
	}
}

func TestS3RetriesAndBackoff(t *testing.T) {
	e := startFakeS3(t, false, nil)
	ctx := context.Background()
	e.failNext.Store(2)
	if err := e.be.Put(ctx, "r", strings.NewReader("v")); err != nil {
		t.Fatalf("put with 2 transient failures: %v", err)
	}
	if got := e.requests.Load(); got != 3 {
		t.Fatalf("requests %d", got)
	}
	e.failStatus.Store(429)
	e.failNext.Store(1)
	if _, err := e.be.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	// Exhaustion: MaxRetries=3 -> 4 attempts, error carries the status.
	e.failStatus.Store(503)
	e.failNext.Store(100)
	before := e.requests.Load()
	err := e.be.Put(ctx, "r2", strings.NewReader("v"))
	var se *S3Error
	if !errors.As(err, &se) || se.Status != 503 || se.Code != "SlowDown" {
		t.Fatalf("got %v", err)
	}
	if e.requests.Load()-before != 4 {
		t.Fatalf("attempts %d", e.requests.Load()-before)
	}
}

func TestS3NoRetryOnClientError(t *testing.T) {
	e := startFakeS3(t, false, nil)
	e.failStatus.Store(400)
	e.failNext.Store(1)
	before := e.requests.Load()
	err := e.be.Put(context.Background(), "x", strings.NewReader("v"))
	var se *S3Error
	if !errors.As(err, &se) || se.Status != 400 || e.requests.Load()-before != 1 {
		t.Fatalf("err=%v attempts=%d", err, e.requests.Load()-before)
	}
}

func TestS3ErrorMapping(t *testing.T) {
	e := startFakeS3(t, false, nil)
	ctx := context.Background()
	// Wrong secret -> 403 SignatureDoesNotMatch as *S3Error (not retried).
	bad, _ := NewS3Backend(S3Config{Endpoint: e.srv.URL, Region: e.region, Bucket: e.bucket, AccessKey: e.ak, SecretKey: "wrong", AddressingStyle: "path"})
	err := bad.Put(ctx, "x", strings.NewReader("v"))
	var se *S3Error
	if !errors.As(err, &se) || se.Status != 403 || se.Code != "SignatureDoesNotMatch" {
		t.Fatalf("got %v", err)
	}
	// Missing bucket is not ErrNotFound.
	nb, _ := NewS3Backend(S3Config{Endpoint: e.srv.URL, Region: e.region, Bucket: "nobucket", AccessKey: e.ak, SecretKey: e.sk, AddressingStyle: "path"})
	_, err = nb.Get(ctx, "x")
	if errors.Is(err, ErrNotFound) || !errors.As(err, &se) || se.Code != "NoSuchBucket" {
		t.Fatalf("got %v", err)
	}
	if _, err = e.be.Get(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = e.be.Delete(ctx, "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestS3SpecialKeysSigned(t *testing.T) {
	e := startFakeS3(t, false, func(c *S3Config) { c.Prefix = "my dir/ü+x~/" })
	ctx := context.Background()
	for _, n := range []string{"a b", "ü€", "x+y=z&w", "star*"} {
		if err := e.be.Put(ctx, n, strings.NewReader("v-"+n)); err != nil {
			t.Fatalf("%q: %v", n, err)
		}
		rc, err := e.be.Get(ctx, n)
		if err != nil {
			t.Fatalf("%q: %v", n, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != "v-"+n {
			t.Fatalf("%q: %q", n, got)
		}
	}
	if l, err := e.be.List(ctx, "x+"); err != nil || len(l) != 1 || l[0].Name != "x+y=z&w" {
		t.Fatalf("%v %+v", err, l)
	}
}

func TestS3ContextCancellation(t *testing.T) {
	e := startFakeS3(t, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.be.Put(ctx, "c", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("put: %v", err)
	}
	if _, err := e.be.Get(ctx, "c"); !errors.Is(err, context.Canceled) {
		t.Fatalf("get: %v", err)
	}
	if _, err := e.be.List(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("list: %v", err)
	}
	if err := e.be.Delete(ctx, "c"); !errors.Is(err, context.Canceled) {
		t.Fatalf("delete: %v", err)
	}
	// Cancellation during backoff stops promptly.
	slow := startFakeS3(t, false, func(c *S3Config) { c.RetryBase = time.Hour; c.MaxRetries = 5 })
	slow.failNext.Store(100)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	start := time.Now()
	if err := slow.be.Put(ctx2, "c", strings.NewReader("x")); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("backoff cancel: %v after %v", err, time.Since(start))
	}
}

func TestS3GetStreamCancel(t *testing.T) {
	e := startFakeS3(t, false, nil)
	if err := e.be.Put(context.Background(), "o", strings.NewReader(strings.Repeat("a", 1<<20))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := e.be.Get(ctx, "o")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, _ = io.CopyN(io.Discard, rc, 10)
	cancel()
	if _, err = io.Copy(io.Discard, rc); err == nil {
		t.Skip("body already fully buffered by the transport")
	}
}

func TestS3TLSVerificationAndCABundle(t *testing.T) {
	f := newFakeS3(t)
	srv := httptest.NewUnstartedServer(f)
	srv.EnableHTTP2 = false
	srv.StartTLS()
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	f.host = u.Host
	cfg := S3Config{Endpoint: srv.URL, Region: f.region, Bucket: f.bucket, AccessKey: f.ak, SecretKey: f.sk,
		AddressingStyle: "path", RetryBase: time.Millisecond, MaxRetries: 1}
	// Default: the test CA is unknown -> must fail verification.
	be, err := NewS3Backend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = be.Put(context.Background(), "t", strings.NewReader("x"))
	var uae x509.UnknownAuthorityError
	if err == nil || !errors.As(err, &uae) {
		t.Fatalf("expected unknown authority, got %v", err)
	}
	// With the CA bundle it works.
	cfg.CAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	be, err = NewS3Backend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = be.Put(context.Background(), "t", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	cfg.CAPEM = []byte("not pem")
	if _, err = NewS3Backend(cfg); err == nil {
		t.Fatal("invalid CA bundle accepted")
	}
	_ = tls.VersionTLS12
}

func TestS3ConfigValidation(t *testing.T) {
	good := S3Config{Region: "r", Bucket: "b", AccessKey: "a", SecretKey: "s"}
	if _, err := NewS3Backend(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*S3Config){
		"no region":     func(c *S3Config) { c.Region = "" },
		"no bucket":     func(c *S3Config) { c.Bucket = "" },
		"no secret":     func(c *S3Config) { c.SecretKey = "" },
		"slash":         func(c *S3Config) { c.Bucket = "a/b" },
		"bad scheme":    func(c *S3Config) { c.Endpoint = "ftp://x" },
		"endpoint path": func(c *S3Config) { c.Endpoint = "https://x/y" },
		"bad style":     func(c *S3Config) { c.AddressingStyle = "weird" },
		"abs prefix":    func(c *S3Config) { c.Prefix = "/x" },
	} {
		c := good
		mut(&c)
		if _, err := NewS3Backend(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	b, _ := NewS3Backend(good)
	if !b.virtual || b.base.Host != "s3.r.amazonaws.com" {
		t.Fatalf("default endpoint: %+v", b.base)
	}
}
