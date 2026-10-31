// Package storage implements the shared, bounded SigV4 transport for R2 control.
package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/xiao0yy/cf-worker-matrix-admin/internal/local"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var Missing = errors.New("object missing")
var Conflict = errors.New("conditional write conflict")

type Client struct {
	Endpoint, Bucket, Region, Access, Secret string
	HTTP                                     *http.Client
}

func FromFile(p string) (*Client, string, error) {
	b, e := local.Private(p)
	if e != nil {
		return nil, "", e
	}
	v, e := local.ParseEnv(b, false)
	if e != nil {
		return nil, "", e
	}
	for k := range v {
		switch k {
		case "S3_ENDPOINT", "S3_BUCKET", "S3_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "CLUSTER_ID":
		default:
			return nil, "", errors.New("unknown R2 configuration key")
		}
	}
	c := &Client{Endpoint: strings.TrimRight(v["S3_ENDPOINT"], "/"), Bucket: v["S3_BUCKET"], Region: v["S3_REGION"], Access: v["AWS_ACCESS_KEY_ID"], Secret: v["AWS_SECRET_ACCESS_KEY"]}
	u, e := url.Parse(c.Endpoint)
	if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || !strings.HasSuffix(u.Hostname(), ".r2.cloudflarestorage.com") || c.Region != "auto" || !local.SafeID(c.Bucket) || !local.SafeID(v["CLUSTER_ID"]) || c.Access == "" || c.Secret == "" {
		return nil, "", errors.New("invalid R2 configuration")
	}
	return c, v["CLUSTER_ID"], nil
}
func mac(k []byte, s string) []byte {
	h := hmac.New(sha256.New, k)
	h.Write([]byte(s))
	return h.Sum(nil)
}
func (c *Client) Request(ctx context.Context, method, key string, body []byte, match string) ([]byte, string, error) {
	if key == "" || strings.Contains(key, "..") || strings.ContainsAny(key, "?%#\r\n\\") || strings.HasPrefix(key, "/") {
		return nil, "", errors.New("invalid object key")
	}
	return c.request(ctx, method, key, nil, body, match)
}
func (c *Client) request(ctx context.Context, method, key string, query url.Values, body []byte, match string) ([]byte, string, error) {
	r, e := http.NewRequestWithContext(ctx, method, c.Endpoint+"/"+c.Bucket+"/"+key, bytes.NewReader(body))
	if e != nil {
		return nil, "", e
	}
	r.URL.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
	stamp := time.Now().UTC().Format("20060102T150405Z")
	day := stamp[:8]
	hash := local.Digest(body)
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	values := map[string]string{"host": r.URL.Host, "x-amz-content-sha256": hash, "x-amz-date": stamp}
	if match != "" {
		name := "if-match"
		if match == "*" {
			name = "if-none-match"
		}
		names = []string{"host", name, "x-amz-content-sha256", "x-amz-date"}
		values[name] = match
	}
	canonical := ""
	for _, n := range names {
		canonical += n + ":" + values[n] + "\n"
		if n != "host" {
			r.Header.Set(n, values[n])
		}
	}
	signed := strings.Join(names, ";")
	scope := day + "/" + c.Region + "/s3/aws4_request"
	raw := method + "\n" + r.URL.EscapedPath() + "\n" + r.URL.RawQuery + "\n" + canonical + "\n" + signed + "\n" + hash
	k := mac(mac(mac(mac([]byte("AWS4"+c.Secret), day), c.Region), "s3"), "aws4_request")
	sig := hex.EncodeToString(mac(k, "AWS4-HMAC-SHA256\n"+stamp+"\n"+scope+"\n"+local.Digest([]byte(raw))))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.Access+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
	r.Header.Set("Cache-Control", "no-store")
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	res, e := h.Do(r)
	if e != nil {
		return nil, "", errors.New("R2 request failed")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, (32<<20)+1))
	if e != nil || len(b) > 32<<20 {
		return nil, "", errors.New("invalid R2 response size")
	}
	switch res.StatusCode {
	case 200, 204:
		return b, res.Header.Get("ETag"), nil
	case 404:
		return nil, "", Missing
	case 409, 412:
		return nil, "", Conflict
	}
	return nil, "", fmt.Errorf("R2 HTTP %d", res.StatusCode)
}

// List is paginated at the transport boundary and rejects foreign keys. Callers
// specify a bounded namespace; metadata, never credentials, is returned.
func (c *Client) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix == "" || strings.Contains(prefix, "..") || strings.HasPrefix(prefix, "/") {
		return nil, errors.New("invalid list prefix")
	}
	var keys []string
	cursor := ""
	for pageCount := 0; ; pageCount++ {
		if pageCount >= 1000 {
			return nil, errors.New("listing pagination limit exceeded")
		}
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if cursor != "" {
			q.Set("continuation-token", cursor)
		}
		b, _, err := c.request(ctx, "GET", "", q, nil, "")
		if err != nil {
			return nil, err
		}
		var page struct {
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken"`
			Contents  []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
		}
		if err = xml.Unmarshal(b, &page); err != nil {
			return nil, err
		}
		for _, v := range page.Contents {
			if !strings.HasPrefix(v.Key, prefix) {
				return nil, errors.New("list returned foreign namespace")
			}
			keys = append(keys, v.Key)
		}
		if len(keys) > 100000 {
			return nil, errors.New("namespace exceeds maintenance inventory limit")
		}
		if !page.Truncated {
			return keys, nil
		}
		if page.Next == "" || page.Next == cursor {
			return nil, errors.New("invalid continuation token")
		}
		cursor = page.Next
	}
}
func (c *Client) Get(ctx context.Context, k string) ([]byte, string, error) {
	return c.Request(ctx, "GET", k, nil, "")
}
func (c *Client) Put(ctx context.Context, k string, b []byte, m string) error {
	_, _, e := c.Request(ctx, "PUT", k, b, m)
	return e
}
func (c *Client) Immutable(ctx context.Context, k string, b []byte) error {
	e := c.Put(ctx, k, b, "*")
	if e != nil && !errors.Is(e, Conflict) {
		return e
	}
	got, _, e := c.Get(ctx, k)
	if e != nil {
		return e
	}
	if !bytes.Equal(got, b) {
		return errors.New("immutable object differs")
	}
	return nil
}
