// Copyright (c) 2020 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.package main

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/minio/cli"
	minio "github.com/minio/minio-go/v6"
	"github.com/minio/minio-go/v6/pkg/credentials"
	xhttp "github.com/minio/minio/cmd/http"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/console"
	xioutil "github.com/minio/minio/pkg/ioutil"
)

const (
	// CacheControl header
	CacheControl = "Cache-Control"
	// Expires header
	Expires = "Expires"

	defaultMinioHealthCheckPath     = "/minio/health/ready"
	defaultMinioHealthCheckDuration = 60 // in seconds

	// EnvCacheEndpoint cache endpoint
	EnvCacheEndpoint = "SIDEKICK_CACHE_ENDPOINT"
	// EnvCacheAccessKey cache access key
	EnvCacheAccessKey = "SIDEKICK_CACHE_ACCESS_KEY"
	// EnvCacheSecretKey cache secret key
	EnvCacheSecretKey = "SIDEKICK_CACHE_SECRET_KEY"
	// EnvCacheBucket bucket to cache to.
	EnvCacheBucket = "SIDEKICK_CACHE_BUCKET"
	// EnvCacheMinSize minimum size of object that should be cached.
	EnvCacheMinSize = "SIDEKICK_CACHE_MIN_SIZE"
	// EnvCacheHealthCheckDuration - health check duration
	EnvCacheHealthCheckDuration = "SIDEKICK_CACHE_HEALTH_DURATION"
)

// S3CacheClient client to S3 cache storage.
type S3CacheClient struct {
	endpoint            string
	useTLS              bool
	api                 *minio.Client
	httpClient          *http.Client
	methods             []string
	bucket              string
	minSize             uint64
	up                  bool
	healthCheckDuration time.Duration
}

func (c *S3CacheClient) isCacheable(method string) bool {
	for _, m := range c.methods {
		if method == m {
			return true
		}
	}
	return false
}

func (c *S3CacheClient) healthCheck() {
	healthCheckURL := strings.TrimSuffix(c.endpoint, slashSeparator) + defaultMinioHealthCheckPath
	for {
		req, err := http.NewRequest(http.MethodGet, healthCheckURL, nil)
		if err != nil {
			if globalLoggingEnabled {
				logMsg(logMessage{Endpoint: c.endpoint, Error: err})
			}
			c.up = false
			time.Sleep(time.Duration(c.healthCheckDuration) * time.Second)
			continue
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			c.httpClient.CloseIdleConnections()
			c.up = false
		} else {
			// Drain the connection.
			io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
			c.up = resp.StatusCode >= http.StatusOK && resp.StatusCode <= http.StatusPartialContent
		}
		time.Sleep(time.Duration(c.healthCheckDuration) * time.Second)
	}
}

type cacheControl struct {
	expires        time.Time
	maxAge         int
	sMaxAge        int
	minFresh       int
	maxStale       int
	noStore        bool
	private        bool
	public         bool
	onlyIfCached   bool
	noCache        bool
	immutable      bool
	mustRevalidate bool
}

// returns struct with cache-control settings from user metadata.
func parseCacheControlHeaders(header http.Header) *cacheControl {
	c := cacheControl{}
	if v, ok := header[Expires]; ok {
		if t, e := time.Parse(http.TimeFormat, strings.Join(v, "")); e == nil {
			c.expires = t.UTC()
		}
	}
	cc, ok := header[CacheControl]
	if !ok && c.expires.Equal(timeZero) {
		return nil
	}
	v := strings.Join(cc, "")
	vals := strings.Split(v, ",")
	for _, val := range vals {
		val = strings.TrimSpace(val)
		if val == "no-store" {
			c.noStore = true
			continue
		}
		if val == "only-if-cached" {
			c.onlyIfCached = true
			continue
		}
		if val == "private" {
			c.private = true
			continue
		}
		if val == "public" {
			c.public = true
			continue
		}
		if val == "no-cache" {
			c.noCache = true
			continue
		}
		if val == "immutable" {
			c.immutable = true
			continue
		}
		if val == "must-revalidate" {
			c.mustRevalidate = true
			continue
		}
		p := strings.Split(val, "=")

		if len(p) != 2 {
			continue
		}
		if p[0] == "max-age" ||
			p[0] == "s-maxage" ||
			p[0] == "min-fresh" ||
			p[0] == "max-stale" {
			i, err := strconv.Atoi(p[1])
			if err != nil {
				return nil
			}
			if p[0] == "max-age" {
				c.maxAge = i
			}
			if p[0] == "s-maxage" {
				c.sMaxAge = i
			}
			if p[0] == "min-fresh" {
				c.minFresh = i
			}
			if p[0] == "max-stale" {
				c.maxStale = i
			}
		}
	}
	return &c
}
func (c *cacheControl) revalidate() bool {
	if c == nil {
		return true
	}
	return c.noCache || c.mustRevalidate
}
func (c *cacheControl) neverCache() bool {
	if c == nil {
		return false
	}
	return c.private || c.noStore
}
func (c *cacheControl) fresh(modTime time.Time) bool {
	if c == nil {
		return false
	}
	stale := c.isStale(modTime)
	return (!stale && !c.revalidate()) || (c.immutable || c.onlyIfCached)
}

func (c *cacheControl) isStale(modTime time.Time) bool {
	if c == nil {
		return false
	}
	// response will never be stale if only-if-cached is set
	if c.onlyIfCached || c.immutable {
		return false
	}
	// Cache-Control value no-store indicates never cache
	if c.noStore {
		return true
	}
	// Cache-Control value no-cache indicates cache entry needs to be revalidated before
	// serving from cache
	if c.noCache {
		return true
	}
	now := time.Now()

	if c.sMaxAge > 0 && c.sMaxAge < int(now.Sub(modTime).Seconds()) {
		return true
	}
	if c.maxAge > 0 && c.maxAge < int(now.Sub(modTime).Seconds()) {
		return true
	}

	if !c.expires.Equal(timeZero) && c.expires.Before(time.Now().Add(time.Duration(c.maxStale))) {
		return true
	}

	if c.minFresh > 0 && c.minFresh <= int(now.Sub(modTime).Seconds()) {
		return true
	}

	return false
}

func setRespHeaders(w http.ResponseWriter, headers http.Header) {
	for k, v := range headers {
		w.Header().Set(k, strings.Join(v, ""))
	}
}

// see https://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopToHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"TE":                  {},
	"Trailers":            {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func getEndToEndHeaders(respHeaders http.Header) http.Header {
	hdrs := respHeaders.Clone()
	for h := range respHeaders {
		if _, ok := hopToHopHeaders[h]; ok {
			delete(hdrs, h)
		}
	}
	return hdrs
}

const (
	amzMetaPrefix = "X-Amz-Meta-"
)

type cacheHeader struct {
	http.Header
}

func getCacheResponseHeaders(oi minio.ObjectInfo) (ch cacheHeader) {
	h := make(http.Header)
	for k, v := range oi.UserMetadata {
		key := strings.TrimPrefix(k, amzMetaPrefix)
		h.Set(key, v)
	}
	ch = cacheHeader{Header: h}
	start, rangeLen, err := ch.Range().GetOffsetLength(oi.Size)
	if err != nil {
		return ch
	}

	// Set content length.
	ch.Header.Set(xhttp.ContentLength, strconv.FormatInt(rangeLen, 10))
	if ch.Range() != nil {
		contentRange := fmt.Sprintf("bytes %d-%d/%d", start, start+rangeLen-1, oi.Size)
		ch.Header.Set(xhttp.ContentRange, contentRange)
	}

	return ch
}

//Expires returns expires header from cached response
func (c cacheHeader) Expires() time.Time {
	if v, ok := c.Header[xhttp.Expires]; ok {
		if t, e := time.Parse(http.TimeFormat, strings.Join(v, "")); e == nil {
			return t.UTC()
		}
	}
	return timeZero
}

//ETag returns ETag from cached response
func (c cacheHeader) ETag() string {
	return c.Header.Get(xhttp.ETag)
}

//LastModified returns last modified header from cached response
func (c cacheHeader) LastModified() time.Time {
	if v, ok := c.Header[xhttp.LastModified]; ok {
		if t, e := time.Parse(http.TimeFormat, strings.Join(v, "")); e == nil {
			return t.UTC()
		}
	}
	return timeZero
}

const (
	byteRangePrefix = "bytes="
)

var errInvalidRange = fmt.Errorf("Invalid range")

// HTTPRangeSpec represents a range specification as supported by S3 GET
// object request.
//
// Case 1: Not present -> represented by a nil RangeSpec
// Case 2: bytes=1-10 (absolute start and end offsets) -> RangeSpec{false, 1, 10}
// Case 3: bytes=10- (absolute start offset with end offset unspecified) -> RangeSpec{false, 10, -1}
// Case 4: bytes=-30 (suffix length specification) -> RangeSpec{true, -30, -1}
type HTTPRangeSpec struct {
	// Does the range spec refer to a suffix of the object?
	IsSuffixLength bool

	// Start and end offset specified in range spec
	Start, End int64
}

// GetLength - get length of range
func (h *HTTPRangeSpec) GetLength(resourceSize int64) (rangeLength int64, err error) {
	switch {
	case resourceSize < 0:
		return 0, errors.New("Resource size cannot be negative")

	case h == nil:
		rangeLength = resourceSize

	case h.IsSuffixLength:
		specifiedLen := -h.Start
		rangeLength = specifiedLen
		if specifiedLen > resourceSize {
			rangeLength = resourceSize
		}

	case h.Start >= resourceSize:
		return 0, errInvalidRange

	case h.End > -1:
		end := h.End
		if resourceSize <= end {
			end = resourceSize - 1
		}
		rangeLength = end - h.Start + 1

	case h.End == -1:
		rangeLength = resourceSize - h.Start

	default:
		return 0, errors.New("Unexpected range specification case")
	}

	return rangeLength, nil
}

// GetOffsetLength computes the start offset and length of the range
// given the size of the resource
func (h *HTTPRangeSpec) GetOffsetLength(resourceSize int64) (start, length int64, err error) {
	if h == nil {
		// No range specified, implies whole object.
		return 0, resourceSize, nil
	}

	length, err = h.GetLength(resourceSize)
	if err != nil {
		return 0, 0, err
	}

	start = h.Start
	if h.IsSuffixLength {
		start = resourceSize + h.Start
		if start < 0 {
			start = 0
		}
	}
	return start, length, nil
}

// Parse a HTTP range header value into a HTTPRangeSpec
func parseRequestRangeSpec(rangeString string) (hrange *HTTPRangeSpec, err error) {
	// Return error if given range string doesn't start with byte range prefix.
	if !strings.HasPrefix(rangeString, byteRangePrefix) {
		return nil, fmt.Errorf("'%s' does not start with '%s'", rangeString, byteRangePrefix)
	}

	// Trim byte range prefix.
	byteRangeString := strings.TrimPrefix(rangeString, byteRangePrefix)

	// Check if range string contains delimiter '-', else return error. eg. "bytes=8"
	sepIndex := strings.Index(byteRangeString, "-")
	if sepIndex == -1 {
		return nil, fmt.Errorf("'%s' does not have a valid range value", rangeString)
	}

	offsetBeginString := byteRangeString[:sepIndex]
	offsetBegin := int64(-1)
	// Convert offsetBeginString only if its not empty.
	if len(offsetBeginString) > 0 {
		if offsetBeginString[0] == '+' {
			return nil, fmt.Errorf("Byte position ('%s') must not have a sign", offsetBeginString)
		} else if offsetBegin, err = strconv.ParseInt(offsetBeginString, 10, 64); err != nil {
			return nil, fmt.Errorf("'%s' does not have a valid first byte position value", rangeString)
		} else if offsetBegin < 0 {
			return nil, fmt.Errorf("First byte position is negative ('%d')", offsetBegin)
		}
	}

	offsetEndString := byteRangeString[sepIndex+1:]
	offsetEnd := int64(-1)
	// Convert offsetEndString only if its not empty.
	if len(offsetEndString) > 0 {
		if offsetEndString[0] == '+' {
			return nil, fmt.Errorf("Byte position ('%s') must not have a sign", offsetEndString)
		} else if offsetEnd, err = strconv.ParseInt(offsetEndString, 10, 64); err != nil {
			return nil, fmt.Errorf("'%s' does not have a valid last byte position value", rangeString)
		} else if offsetEnd < 0 {
			return nil, fmt.Errorf("Last byte position is negative ('%d')", offsetEnd)
		}
	}

	switch {
	case offsetBegin > -1 && offsetEnd > -1:
		if offsetBegin > offsetEnd {
			return nil, errInvalidRange
		}
		return &HTTPRangeSpec{false, offsetBegin, offsetEnd}, nil
	case offsetBegin > -1:
		return &HTTPRangeSpec{false, offsetBegin, -1}, nil
	case offsetEnd > -1:
		if offsetEnd == 0 {
			return nil, errInvalidRange
		}
		return &HTTPRangeSpec{true, -offsetEnd, -1}, nil
	default:
		// rangeString contains first and last byte positions missing. eg. "bytes=-"
		return nil, fmt.Errorf("'%s' does not have valid range value", rangeString)
	}
}

// String returns stringified representation of range for a particular resource size.
func (h *HTTPRangeSpec) String(resourceSize int64) string {
	if h == nil {
		return ""
	}
	off, length, err := h.GetOffsetLength(resourceSize)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", off, off+length-1)
}
func (c cacheHeader) Range() (rs *HTTPRangeSpec) {
	if rangeHeader, ok := c.Header["Range"]; ok {
		if len(rangeHeader) != 0 {
			var err error
			rs, err = parseRequestRangeSpec(strings.Join(rangeHeader, ""))
			if err != nil {
				return nil
			}
		}
		return rs
	}
	return nil
}

func getPutOpts(h http.Header) minio.PutObjectOptions {
	var opts minio.PutObjectOptions
	opts.UserMetadata = make(map[string]string)
	for k, v := range h {
		key := fmt.Sprintf("%s%s", amzMetaPrefix, k)
		opts.UserMetadata[key] = strings.Join(v, "")
	}
	return opts
}
func isFresh(cacheCC, reqCC *cacheControl, lastModified time.Time) bool {
	if cacheCC == nil && reqCC == nil {
		return false
	}
	freshCache := cacheCC.fresh(lastModified)
	freshReq := reqCC.fresh(lastModified)

	if reqCC == nil {
		return freshCache
	}
	if cacheCC == nil {
		return freshReq
	}
	return freshCache && freshReq
}

func cacheHandler(next http.HandlerFunc, w http.ResponseWriter, r *http.Request, b *Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clnt := b.cacheClient
		if clnt == nil || !clnt.isCacheable(r.Method) || !clnt.up {
			next.ServeHTTP(w, r)
			return
		}

		sortURLParams(r.URL)
		key := generateKey(r.URL, r.Host)
		coreClient := minio.Core{Client: clnt.api}
		var opts minio.GetObjectOptions
		for k, v := range r.Header {
			opts.Set(k, strings.Join(v, ""))
		}

		reader, oi, _, cacheErr := coreClient.GetObject(clnt.bucket, key, opts)
		cacheHdrs := getCacheResponseHeaders(oi)
		cc := parseCacheControlHeaders(cacheHdrs.Header)
		reqCC := parseCacheControlHeaders(r.Header)
		serveCache := false
		if cacheErr == nil {
			// write response headers and response output return
			defer reader.Close()
			if reqCC.neverCache() {
				// for expired content, revert to backend and clear cache.
				next.ServeHTTP(w, r)
				return
			}
			serveCache = isFresh(cc, reqCC, cacheHdrs.LastModified())
			if !serveCache {
				// set cache headers for ETag and LastModified verification
				if r.Header.Get(xhttp.ETag) == "" && cacheHdrs.ETag() != "" {
					r.Header.Set(xhttp.IfNoneMatch, cacheHdrs.ETag())
				}
				if r.Header.Get(xhttp.LastModified) == "" && !cacheHdrs.LastModified().IsZero() {
					r.Header.Set(xhttp.IfModifiedSince, cacheHdrs.LastModified().UTC().Format(http.TimeFormat))
				}
			}
		}
		if r.Method == http.MethodHead {
			if cacheErr != nil || !serveCache {
				next.ServeHTTP(w, r)
				return
			}
			for k, v := range cacheHdrs.Header {
				w.Header().Set(k, strings.Join(v, ","))
			}
			if cacheHdrs.Range() != nil {
				w.WriteHeader(http.StatusPartialContent)
			} else {
				w.WriteHeader(http.StatusOK)
			}
			return
		}
		var result *http.Response
		var rec *httptest.ResponseRecorder
		if r.Method == http.MethodGet {
			// either no cached entry or cache entry requires revalidation
			if !serveCache {
				rec = httptest.NewRecorder()
				next.ServeHTTP(rec, r)
				result = rec.Result()
				statusCode := result.StatusCode
				if cacheErr != nil && reqCC != nil && reqCC.onlyIfCached {
					// need to issue a gateway timeout response here.
					w.WriteHeader(http.StatusGatewayTimeout)
					return
				}
				if statusCode == http.StatusNotModified {
					// add end to headers to cache response headers
					// serve from cache
					serveCache = true
					endHdrs := getEndToEndHeaders(result.Header)
					for k, v := range endHdrs {
						cacheHdrs.Header[k] = v
					}
				} else if statusCode != http.StatusOK {
					go func() {
						clnt.api.RemoveObject(clnt.bucket, key)
					}()
					// write backend response and return
				}
			}
			if serveCache {
				// serve cache entry
				setRespHeaders(w, cacheHdrs.Header)
				statusCodeWritten := false
				if cacheHdrs.Range() != nil {
					w.WriteHeader(http.StatusPartialContent)
				}
				httpWriter := xioutil.WriteOnClose(w)
				// Write object content to response body
				if _, err := io.Copy(httpWriter, reader); err != nil {
					if !httpWriter.HasWritten() && !statusCodeWritten { // write error response only if no data or headers has been written to client yet
						next.ServeHTTP(w, r)
					}
					return
				}

				if err := httpWriter.Close(); err != nil {
					if !httpWriter.HasWritten() && !statusCodeWritten { // write error response only if no data or headers has been written to client yet
						next.ServeHTTP(w, r)
						return
					}
				}
				return
			}
			for k, v := range result.Header {
				w.Header().Set(k, strings.Join(v, ","))
			}
			value := rec.Body.Bytes()
			statusCode := result.StatusCode
			w.WriteHeader(statusCode)
			w.Write(value)
			respCC := parseCacheControlHeaders(result.Header)
			if respCC.neverCache() || len(value) < int(clnt.minSize) {
				if cacheErr == nil {
					go func() {
						clnt.api.RemoveObject(clnt.bucket, key)
					}()
				}
				return
			}
			rs := result.Header.Get(xhttp.ContentRange)
			if rs != "" {
				// Avoid caching range GET's for now.
				if cacheErr == nil {
					go func() {
						clnt.api.RemoveObject(clnt.bucket, key)
					}()
				}
				return
			}

			go func() {
				opts := getPutOpts(result.Header)
				_, err := clnt.api.PutObject(clnt.bucket, key, bytes.NewReader(value), int64(len(value)), opts)
				if err != nil {
					clnt.up = false
					logger.LogIf(context.Background(), err, "Failed to cache object")
				}
			}()
			return
		}
	}
}

type cacheConfig struct {
	endpoint  string
	useTLS    bool
	accessKey string
	secretKey string
	bucket    string
	minSize   uint64
	duration  time.Duration
}

func newCacheConfig() *cacheConfig {
	cURL := os.Getenv(EnvCacheEndpoint)
	if cURL == "" {
		return nil
	}
	accessKey := os.Getenv(EnvCacheAccessKey)
	secretKey := os.Getenv(EnvCacheSecretKey)
	bucket := os.Getenv(EnvCacheBucket)
	if accessKey == "" || secretKey == "" || bucket == "" {
		console.Fatalln(fmt.Errorf("One or more of AccessKey:%s SecretKey: %s Bucket:%s missing", accessKey, secretKey, bucket), "Missing cache configuration")
	}
	minSizeStr := os.Getenv(EnvCacheMinSize)
	var minSize uint64
	var err error
	if minSizeStr != "" {
		minSize, err = humanize.ParseBytes(minSizeStr)
		if err != nil {
			console.Fatalln(fmt.Errorf("Unable to parse SIDEKICK_CACHE_MIN_SIZE %s should be in human readable units such as 64MB", minSizeStr))
		}
	}
	durationStr := os.Getenv(EnvCacheHealthCheckDuration)
	duration := defaultMinioHealthCheckDuration
	if durationStr != "" {
		duration, err = strconv.Atoi(durationStr)
		if err != nil {
			console.Fatalln(fmt.Errorf("Unable to parse SIDEKICK_CACHE_HEALTH_DURATION %s should be an integer", durationStr))
		}
	}
	return &cacheConfig{endpoint: cURL,
		accessKey: accessKey,
		secretKey: secretKey,
		bucket:    bucket,
		minSize:   minSize,
		duration:  time.Duration(duration) * time.Second,
	}
}
func newCacheClient(ctx *cli.Context, cfg *cacheConfig) *S3CacheClient {
	if cfg == nil {
		return nil
	}
	creds := credentials.NewStaticV4(cfg.accessKey, cfg.secretKey, "")
	var e error
	s3Clnt := &S3CacheClient{}
	options := minio.Options{
		Creds:        creds,
		Secure:       cfg.useTLS,
		Region:       "",
		BucketLookup: 0,
	}
	u, err := url.Parse(cfg.endpoint)
	if err != nil {
		return nil
	}
	api, e := minio.NewWithOptions(u.Host, &options)
	if e != nil {
		return nil
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 10 * time.Second,
		// Set this value so that the underlying transport round-tripper
		// doesn't try to auto decode the body of objects with
		// content-encoding set to `gzip`.
		//
		// Refer:
		//    https://golang.org/src/net/http/transport.go?h=roundTrip#L1843
		DisableCompression: true,
	}

	if cfg.useTLS {
		// Keep TLS config.
		tlsConfig := &tls.Config{
			RootCAs: mustGetSystemCertPool(),
			// Can't use SSLv3 because of POODLE and BEAST
			// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
			// Can't use TLSv1.1 because of RC4 cipher usage
			MinVersion:         tls.VersionTLS12,
			NextProtos:         []string{"http/1.1"},
			InsecureSkipVerify: ctx.GlobalBool("insecure"),
		}
		tr.TLSClientConfig = tlsConfig
	}

	var transport http.RoundTripper = tr
	// Set the new transport.
	api.SetCustomTransport(transport)
	// Set app info.
	api.SetAppInfo(ctx.App.Name, ctx.App.Version)
	// Store the new api object.
	s3Clnt.api = api
	cfg.endpoint = strings.TrimSuffix(cfg.endpoint, slashSeparator)

	target, err := url.Parse(cfg.endpoint)
	if err != nil {
		console.Fatalln(fmt.Errorf("Unable to parse input arg %s: %s", cfg.endpoint, err))
	}
	if target.Scheme == "" {
		target.Scheme = "http"
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		console.Fatalln("Unexpected scheme %s, should be http or https, please use '%s --help'",
			cfg.endpoint, ctx.App.Name)
	}
	if target.Host == "" {
		console.Fatalln(fmt.Errorf("Missing host address %s, please use '%s --help'",
			cfg.endpoint, ctx.App.Name))
	}
	s3Clnt.methods = []string{http.MethodGet, http.MethodHead}
	s3Clnt.bucket = cfg.bucket
	s3Clnt.minSize = cfg.minSize
	s3Clnt.healthCheckDuration = cfg.duration
	s3Clnt.endpoint = cfg.endpoint
	s3Clnt.useTLS = target.Scheme == "https"
	s3Clnt.httpClient = &http.Client{Transport: tr}
	go s3Clnt.healthCheck()
	return s3Clnt
}

func sortURLParams(URL *url.URL) {
	params := URL.Query()
	for _, param := range params {
		sort.Slice(param, func(i, j int) bool {
			return param[i] < param[j]
		})
	}
	URL.RawQuery = params.Encode()
}

func generateKey(u *url.URL, host string) string {
	hash := sha256.New()
	hash.Write([]byte(host))
	hash.Write([]byte(u.String()))
	hashSum := hex.EncodeToString(hash.Sum(nil))
	return hashSum[0:2] + slashSeparator + hashSum[2:4] + slashSeparator + hashSum
}
