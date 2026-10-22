// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3Config configures an S3 or S3-compatible destination (B01/B07).
type S3Config struct {
	// Endpoint is the service URL, for example "https://minio.example:9000".
	// Empty means AWS: https://s3.<Region>.amazonaws.com.
	Endpoint string
	Region   string // required
	Bucket   string // required
	// Prefix is prepended to every object key (for example "cluster-a/").
	Prefix string
	// AddressingStyle is "path", "virtual" or "" (virtual for the default AWS
	// endpoint, path for a custom endpoint).
	AddressingStyle string

	AccessKey    string
	SecretKey    string
	SessionToken string

	// CAPEM optionally adds CA certificates to the system pool. TLS
	// verification is always on; there is deliberately no way to disable it.
	CAPEM []byte
	// HTTPClient replaces the default client (tests). CAPEM is ignored then.
	HTTPClient *http.Client

	// PartSize is the multipart part size and the size above which Put uses
	// multipart upload. Memory use of Put is bounded by one part. Default
	// 16 MiB; S3 requires at least 5 MiB for real services.
	PartSize int64
	// MaxRetries is the number of retries for 5xx, 429 and transport errors
	// (default 4). RetryBase is the first backoff (default 200ms, doubled up
	// to 10s, with jitter).
	MaxRetries int
	RetryBase  time.Duration
}

// S3Error is returned for non-success responses that are not mapped to a
// sentinel error.
type S3Error struct {
	Status  int
	Code    string
	Message string
}

func (e *S3Error) Error() string {
	return fmt.Sprintf("backup: s3 status %d code %q: %s", e.Status, e.Code, e.Message)
}

// S3Backend stores objects in an S3 bucket using a SigV4 client built on the
// standard library.
type S3Backend struct {
	cfg     S3Config
	base    *url.URL
	hc      *http.Client
	now     func() time.Time
	virtual bool
}

const (
	defaultS3PartSize = 16 << 20
	maxS3Parts        = 10000
)

// NewS3Backend validates cfg and returns a backend. It does no network I/O.
func NewS3Backend(cfg S3Config) (*S3Backend, error) {
	if cfg.Region == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("backup: s3 config needs region, bucket, access key and secret key")
	}
	if strings.ContainsAny(cfg.Bucket, "/\\ \x00") {
		return nil, fmt.Errorf("backup: invalid bucket name %q", cfg.Bucket)
	}
	if strings.ContainsRune(cfg.Prefix, 0) || strings.HasPrefix(cfg.Prefix, "/") {
		return nil, errors.New("backup: invalid s3 prefix")
	}
	custom := cfg.Endpoint != ""
	ep := cfg.Endpoint
	if !custom {
		ep = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	u, err := url.Parse(ep)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("backup: invalid s3 endpoint %q", ep)
	}
	b := &S3Backend{cfg: cfg, base: &url.URL{Scheme: u.Scheme, Host: u.Host}, now: time.Now}
	switch cfg.AddressingStyle {
	case "":
		b.virtual = !custom
	case "virtual":
		b.virtual = true
	case "path":
	default:
		return nil, fmt.Errorf("backup: invalid s3 addressing style %q", cfg.AddressingStyle)
	}
	if b.cfg.PartSize <= 0 {
		b.cfg.PartSize = defaultS3PartSize
	}
	if b.cfg.MaxRetries <= 0 {
		b.cfg.MaxRetries = 4
	}
	if b.cfg.RetryBase <= 0 {
		b.cfg.RetryBase = 200 * time.Millisecond
	}
	b.hc = cfg.HTTPClient
	if b.hc == nil {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if len(cfg.CAPEM) > 0 {
			pool, perr := x509.SystemCertPool()
			if perr != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(cfg.CAPEM) {
				return nil, errors.New("backup: s3 CA bundle contains no certificate")
			}
			tc.RootCAs = pool
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = tc
		b.hc = &http.Client{Transport: tr}
	}
	return b, nil
}

func (b *S3Backend) key(name string) string { return b.cfg.Prefix + name }

// newRequest builds a signed request. key may be empty (bucket-level calls).
func (b *S3Backend) newRequest(ctx context.Context, method, key string, query map[string]string, body []byte, hdr http.Header) (*http.Request, error) {
	u := *b.base
	var path string
	if b.virtual {
		u.Host = b.cfg.Bucket + "." + u.Host
		path = "/" + key
	} else {
		path = "/" + b.cfg.Bucket + "/" + key
		if key == "" {
			path = "/" + b.cfg.Bucket
		}
	}
	enc := uriEncode(path, true)
	u.Path, _ = url.PathUnescape(enc)
	u.RawPath = enc
	u.RawQuery = canonicalQuery(query)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	ph := emptySHA256
	if body != nil {
		ph = sha256Hex(body)
		req.ContentLength = int64(len(body))
	}
	signRequest(req, ph, b.now(), b.cfg.Region, b.cfg.AccessKey, b.cfg.SecretKey, b.cfg.SessionToken)
	return req, nil
}

func retryable(status int) bool { return status >= 500 || status == 429 }

func (b *S3Backend) sleep(ctx context.Context, attempt int) error {
	d := b.cfg.RetryBase << attempt
	if d <= 0 || d > 10*time.Second {
		d = 10 * time.Second
	}
	d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// do sends the request, retrying 5xx, 429 and transport errors with backoff.
// The returned response is not an error status unless it is a non-retryable
// 4xx, which the caller maps. Retries are exhausted into an *S3Error or the
// transport error.
func (b *S3Backend) do(ctx context.Context, method, key string, query map[string]string, body []byte, hdr http.Header) (*http.Response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := b.newRequest(ctx, method, key, query, body, hdr)
		if err != nil {
			return nil, err
		}
		resp, err := b.hc.Do(req)
		switch {
		case err != nil:
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			lastErr = err
		case retryable(resp.StatusCode):
			lastErr = readS3Error(resp)
			_ = resp.Body.Close()
		default:
			return resp, nil
		}
		if attempt >= b.cfg.MaxRetries {
			return nil, fmt.Errorf("backup: s3 %s %s failed after %d attempts: %w", method, key, attempt+1, lastErr)
		}
		if err := b.sleep(ctx, attempt); err != nil {
			return nil, err
		}
	}
}

type s3ErrorBody struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// readS3Error reads (a bounded amount of) the body of a failed response.
func readS3Error(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var eb s3ErrorBody
	_ = xml.Unmarshal(data, &eb)
	return &S3Error{Status: resp.StatusCode, Code: eb.Code, Message: eb.Message}
}

// mapStatus converts a final response into an error, or nil for 2xx. The
// body is closed on error.
func mapStatus(resp *http.Response) error {
	if resp.StatusCode/100 == 2 {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		e := readS3Error(resp)
		var se *S3Error
		if errors.As(e, &se) && (se.Code == "NoSuchBucket" || se.Code == "NoSuchUpload") {
			return e
		}
		return ErrNotFound
	}
	return readS3Error(resp)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// Put implements Backend. Objects up to PartSize use one PUT; larger streams
// use multipart upload with one part buffered at a time.
func (b *S3Backend) Put(ctx context.Context, name string, r io.Reader) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r = &ctxReader{ctx, r}
	buf := make([]byte, 0, min(b.cfg.PartSize, 1<<20))
	buf, err := readUpTo(r, buf, b.cfg.PartSize+1)
	if err != nil {
		return err
	}
	key := b.key(name)
	if int64(len(buf)) <= b.cfg.PartSize {
		return b.putSingle(ctx, key, buf)
	}
	return b.putMultipart(ctx, key, buf, r)
}

// readUpTo appends up to n bytes from r to buf (fewer only at EOF).
func readUpTo(r io.Reader, buf []byte, n int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, n-int64(len(buf))))
	return append(buf, data...), err
}

func (b *S3Backend) putSingle(ctx context.Context, key string, data []byte) error {
	resp, err := b.do(ctx, http.MethodPut, key, nil, data, nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	return mapStatus(resp)
}

type completePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeUpload struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []completePart `xml:"Part"`
}

func (b *S3Backend) putMultipart(ctx context.Context, key string, first []byte, r io.Reader) (err error) {
	resp, err := b.do(ctx, http.MethodPost, key, map[string]string{"uploads": ""}, []byte{}, nil)
	if err != nil {
		return err
	}
	if err = mapStatus(resp); err != nil {
		return err
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	derr := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&init)
	drain(resp)
	if derr != nil || init.UploadID == "" {
		return fmt.Errorf("backup: s3 initiate multipart: bad response: %v", derr)
	}
	id := init.UploadID
	defer func() {
		if err != nil {
			// Abort with a fresh context so cancellation still cleans up.
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if ar, aerr := b.do(actx, http.MethodDelete, key, map[string]string{"uploadId": id}, nil, nil); aerr == nil {
				drain(ar)
			}
		}
	}()

	var parts []completePart
	buf := first
	for n := 1; ; n++ {
		if n > maxS3Parts {
			return fmt.Errorf("backup: s3 upload exceeds %d parts; increase PartSize", maxS3Parts)
		}
		if buf, err = readUpTo(r, buf, b.cfg.PartSize); err != nil {
			return err
		}
		chunk := buf[:min(int64(len(buf)), b.cfg.PartSize)]
		if len(chunk) == 0 {
			break
		}
		etag, perr := b.uploadPart(ctx, key, id, n, chunk)
		if perr != nil {
			return perr
		}
		parts = append(parts, completePart{PartNumber: n, ETag: etag})
		buf = append(buf[:0], buf[len(chunk):]...)
		if int64(len(chunk)) < b.cfg.PartSize {
			break
		}
	}
	body, _ := xml.Marshal(completeUpload{Parts: parts})
	cr, err := b.do(ctx, http.MethodPost, key, map[string]string{"uploadId": id}, body, nil)
	if err != nil {
		return err
	}
	if err = mapStatus(cr); err != nil {
		return err
	}
	// CompleteMultipartUpload can return 200 with an error document.
	data, _ := io.ReadAll(io.LimitReader(cr.Body, 1<<20))
	_ = cr.Body.Close()
	if bytes.Contains(data, []byte("<Error>")) {
		var eb s3ErrorBody
		_ = xml.Unmarshal(data, &eb)
		err = &S3Error{Status: 200, Code: eb.Code, Message: eb.Message}
		return err
	}
	return nil
}

func (b *S3Backend) uploadPart(ctx context.Context, key, id string, n int, data []byte) (string, error) {
	q := map[string]string{"partNumber": strconv.Itoa(n), "uploadId": id}
	resp, err := b.do(ctx, http.MethodPut, key, q, data, nil)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if err := mapStatus(resp); err != nil {
		return "", err
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", errors.New("backup: s3 upload part: missing ETag")
	}
	return etag, nil
}

// Get implements Backend. The body is streamed; cancel ctx to abort it.
func (b *S3Backend) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	resp, err := b.do(ctx, http.MethodGet, b.key(name), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := mapStatus(resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

type listResult struct {
	IsTruncated bool   `xml:"IsTruncated"`
	NextToken   string `xml:"NextContinuationToken"`
	Contents    []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

// List implements Backend using ListObjectsV2 with continuation tokens.
func (b *S3Backend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []ObjectInfo
	token := ""
	for {
		q := map[string]string{"list-type": "2", "prefix": b.key(prefix)}
		if token != "" {
			q["continuation-token"] = token
		}
		resp, err := b.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		if err := mapStatus(resp); err != nil {
			return nil, err
		}
		var lr listResult
		derr := xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&lr)
		drain(resp)
		if derr != nil {
			return nil, fmt.Errorf("backup: s3 list: bad response: %w", derr)
		}
		for _, c := range lr.Contents {
			n := strings.TrimPrefix(c.Key, b.cfg.Prefix)
			if n == "" || strings.Contains(n, "/") {
				continue // not one of our flat objects
			}
			out = append(out, ObjectInfo{Name: n, Size: c.Size, ModTime: c.LastModified})
		}
		if !lr.IsTruncated || lr.NextToken == "" {
			break
		}
		token = lr.NextToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete implements Backend. S3 DeleteObject succeeds for missing keys, so a
// HEAD first maps absence to ErrNotFound (not atomic with the delete).
func (b *S3Backend) Delete(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	key := b.key(name)
	resp, err := b.do(ctx, http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		drain(resp)
		return ErrNotFound
	}
	if err := mapStatus(resp); err != nil {
		return err
	}
	drain(resp)
	resp, err = b.do(ctx, http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	return mapStatus(resp)
}

var _ Backend = (*S3Backend)(nil)
