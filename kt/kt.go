package kt

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync/atomic"
	"time"

	"crypto/tls"
	"crypto/x509"
	"encoding/pem"

	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
)

var certExpiryTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "ktrpc_client_certificate_expiry_timestamp",
	Help: "The certificate expiry timestamp (UNIX epoch UTC) labeled by the certificate serial number",
},
	[]string{
		"serial",
	},
)

func init() {
	prometheus.MustRegister(certExpiryTimestamp)
}

const DEFAULT_TIMEOUT = 2 * time.Second

// Error is returned by all functions in this package.
type Error struct {
	// Error returned by KT
	Message string
	// HTTP status code, if any (0 otherwise)
	Code int
}

func (e *Error) Error() string {
	return fmt.Sprintln("kt:", e.Message)
}

// IsError returns true if the error was generated by this package.
func IsError(err error) bool {
	_, ok := err.(*Error)
	return ok
}

// Conn represents a connection to a kyoto tycoon endpoint.
// It uses a connection pool to efficiently communicate with the server.
// Conn is safe for concurrent use.
type Conn struct {
	// Has to be first for atomic alignment
	retryCount uint64
	scheme     string
	timeout    time.Duration
	host       string
	transport  *http.Transport
}

func expiryCertMetric(certFile string) error {
	leftOverCert, err := ioutil.ReadFile(certFile)
	if err != nil {
		return err
	}

	var cert *pem.Block

	for {
		// Some part of this bloc come from the go standard library

		// Several cert can be concatenated in the same file
		cert, leftOverCert = pem.Decode(leftOverCert)

		if cert == nil {
			// The end of the cert list
			return nil
		}

		if cert.Type != "CERTIFICATE" || len(cert.Headers) != 0 {
			// This is is from src/crypto/x509/cert_pool.go
			continue
		}

		xc, err := x509.ParseCertificate(cert.Bytes)
		if err != nil {
			return err
		}

		serial := (*xc.SerialNumber).String()
		expiry := xc.NotAfter

		b := certExpiryTimestamp.WithLabelValues(serial)
		b.Set(float64(expiry.Unix()))
	}
}

func loadCerts(creds string) (*tls.Certificate, *x509.CertPool, error) {
	cert := path.Join(creds, "service.pem")
	key := path.Join(creds, "service-key.pem")
	ca := path.Join(creds, "ca.pem")

	err := expiryCertMetric(cert)
	if err != nil {
		return nil, nil, err
	}

	certX509, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, nil, err
	}

	err = expiryCertMetric(ca)
	if err != nil {
		return nil, nil, err
	}

	caFile, err := ioutil.ReadFile(ca)
	if err != nil {
		return nil, nil, err
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caFile)

	return &certX509, roots, err
}

func newTLSClientConfig(creds string) (*tls.Config, error) {
	certX509, roots, err := loadCerts(creds)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{*certX509},
		RootCAs:      roots,
	}, nil
}

// KT has 2 interfaces, A restful one and an RPC one.
// The RESTful interface is usually much faster than
// the RPC one, but not all methods are implemented.
// Use the RESTFUL interfaces when we can and fallback
// to the RPC one when needed.
//
// The RPC format uses tab separated values with a choice of encoding
// for each of the fields. We use base64 since it is always safe.
//
// REST format is just the body of the HTTP request being the value.

func newConn(host string, port int, poolsize int, timeout time.Duration, creds string) (*Conn, error) {
	var tlsConfig *tls.Config
	var err error

	scheme := "http"

	if creds != "" {
		tlsConfig, err = newTLSClientConfig(creds)
		if err != nil {
			return nil, err
		}

		scheme = "https"
	}

	portstr := strconv.Itoa(port)
	c := &Conn{
		scheme:  scheme,
		timeout: timeout,
		host:    net.JoinHostPort(host, portstr),
		transport: &http.Transport{
			TLSClientConfig:       tlsConfig,
			ResponseHeaderTimeout: timeout,
			MaxIdleConnsPerHost:   poolsize,
		},
	}

	// connectivity check so that we can bail out
	// early instead of when we do the first operation.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _, err = c.doRPC(ctx, "/rpc/void", nil)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// NewConnTLS creates a TLS enabled connection to a Kyoto Tycoon endpoing
func NewConnTLS(host string, port int, poolsize int, timeout time.Duration, creds string) (*Conn, error) {
	return newConn(host, port, poolsize, timeout, creds)
}

// NewConn creates a connection to an Kyoto Tycoon endpoint.
func NewConn(host string, port int, poolsize int, timeout time.Duration) (*Conn, error) {
	return newConn(host, port, poolsize, timeout, "")
}

var (
	ErrTimeout error = &Error{Message: "operation timeout"}
	// the wording on this error is deliberately weird,
	// because users would search for the string logical inconsistency
	// in order to find lookup misses.
	ErrNotFound = &Error{Message: "entry not found aka logical inconsistency"}
	// old gokabinet returned this error on success. Keeping around "for compatibility" until
	// I can kill it with fire.
	ErrSuccess = &Error{Message: "success"}
)

// RetryCount is the number of retries performed due to the remote end
// closing idle connections.
//
// The value increases monotonically, until it wraps to 0.
func (c *Conn) RetryCount() uint64 {
	return atomic.LoadUint64(&c.retryCount)
}

// Count returns the number of records in the database
func (c *Conn) Count(ctx context.Context) (int, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc Count")
	defer span.Finish()
	span.SetTag("url", "/rpc/status")

	code, m, err := c.doRPC(ctx, "/rpc/status", nil)
	if err != nil {
		span.SetTag("status", err)
		return 0, err
	}

	if code != 200 {
		err := makeError(m)
		span.SetTag("status", err)
		return 0, err
	}
	return strconv.Atoi(string(findRec(m, "count").Value))
}

func (c *Conn) remove(ctx context.Context, key string) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc Remove")
	defer span.Finish()

	code, body, err := c.doREST(ctx, "DELETE", key, nil)
	if err != nil {
		span.SetTag("status", err)
		return err
	}
	if code == 404 {
		span.SetTag("status", "not_found")
		return ErrNotFound
	}
	if code != 204 {
		err := &Error{string(body), code}
		span.SetTag("status", err)
		return err
	}
	return nil
}

// GetBulk retrieves the keys in the map. The results will be filled in on function return.
// If a key was not found in the database, it will be removed from the map.
func (c *Conn) GetBulk(ctx context.Context, keysAndVals map[string]string) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc GetBulk")
	defer span.Finish()

	m := make(map[string][]byte)
	for k := range keysAndVals {
		m[k] = zeroslice
	}
	err := c.doGetBulkBytes(ctx, m)
	if err != nil {
		span.SetTag("status", err)
		return err
	}
	for k := range keysAndVals {
		b, ok := m[k]
		if ok {
			keysAndVals[k] = string(b)
		} else {
			delete(keysAndVals, k)
		}
	}
	return nil
}

// Get retrieves the data stored at key. ErrNotFound is
// returned if no such data exists
func (c *Conn) Get(ctx context.Context, key string) (string, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc Get")
	defer span.Finish()
	span.SetTag("key", key)
	s, err := c.doGet(ctx, key)
	if err != nil {
		return "", err
	}
	return string(s), nil
}

// doGet perform http request to retrieve the value associated with key
func (c *Conn) doGet(ctx context.Context, key string) ([]byte, error) {
	span := opentracing.SpanFromContext(ctx)

	code, body, err := c.doREST(ctx, "GET", key, nil)
	if err != nil {
		span.SetTag("err", err)
		return nil, err
	}

	switch code {
	case 200:
		span.SetTag("status", "ok")
		break
	case 404:
		span.SetTag("status", "not_found")
		return nil, ErrNotFound
	default:
		err := &Error{string(body), code}
		span.SetTag("status", err)
		return nil, err
	}
	return body, nil
}

// GetBytes retrieves the data stored at key in the format of a byte slice
// ErrNotFound is returned if no such data is found.
func (c *Conn) GetBytes(ctx context.Context, key string) ([]byte, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc GetBytes")
	defer span.Finish()
	span.SetTag("key", key)
	return c.doGet(ctx, key)
}

// Set stores the data at key
func (c *Conn) set(ctx context.Context, key string, value []byte) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc Set")
	defer span.Finish()

	code, body, err := c.doREST(ctx, "PUT", key, value)
	if err != nil {
		return err
	}
	if code != 201 {
		return &Error{string(body), code}
	}

	return nil
}

var zeroslice = []byte("0")

// GetBulkBytes retrieves the keys in the map. The results will be filled in on function return.
// If a key was not found in the database, it will be removed from the map.
func (c *Conn) GetBulkBytes(ctx context.Context, keys map[string][]byte) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc GetBulkBytes")
	defer span.Finish()
	err := c.doGetBulkBytes(ctx, keys)
	if err != nil {
		span.SetTag("status", err)
	}
	return err
}

// doGetBulkBytes retrieves the keys in the map. The results will be filled in on function return.
// If a key was not found in the database, it will be removed from the map.
func (c *Conn) doGetBulkBytes(ctx context.Context, keys map[string][]byte) error {

	// The format for querying multiple keys in KT is to send a
	// TSV value for each key with a _ as a prefix.
	// KT then returns the value as a TSV set with _ in front of the keys
	keystransmit := make([]KV, 0, len(keys))
	for k, _ := range keys {
		// we set the value to nil because we want a sentinel value
		// for when no data was found. This is important for
		// when we remove the not found keys from the map
		keys[k] = nil
		keystransmit = append(keystransmit, KV{"_" + k, zeroslice})
	}

	code, m, err := c.doRPC(ctx, "/rpc/get_bulk", keystransmit)
	if err != nil {
		return err
	}
	if code != 200 {
		return makeError(m)
	}
	for _, kv := range m {
		if kv.Key[0] != '_' {
			continue
		}
		keys[kv.Key[1:]] = kv.Value
	}
	for k, v := range keys {
		if v == nil {
			delete(keys, k)
		}
	}
	return nil
}

// SetBulk stores the values in the map.
func (c *Conn) setBulk(ctx context.Context, values map[string]string) (int64, error) {
	vals := make([]KV, 0, len(values))
	for k, v := range values {
		vals = append(vals, KV{"_" + k, []byte(v)})
	}
	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc SetBulk")
	defer span.Finish()

	code, m, err := c.doRPC(ctx, "/rpc/set_bulk", vals)
	if err != nil {
		span.SetTag("status", err)
		return 0, err
	}
	if code != 200 {
		span.SetTag("status", code)
		return 0, makeError(m)
	}
	return strconv.ParseInt(string(findRec(m, "num").Value), 10, 64)
}

func (c *Conn) removeBulk(ctx context.Context, keys []string) (int64, error) {
	vals := make([]KV, 0, len(keys))
	for _, k := range keys {
		vals = append(vals, KV{"_" + k, zeroslice})
	}

	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc RemoveBulk")
	defer span.Finish()

	code, m, err := c.doRPC(ctx, "/rpc/remove_bulk", vals)
	if err != nil {
		span.SetTag("status", err)
		return 0, err
	}
	if code != 200 {
		span.SetTag("status", code)
		return 0, makeError(m)
	}
	return strconv.ParseInt(string(findRec(m, "num").Value), 10, 64)
}

// MatchPrefix performs the match_prefix operation against the server
// It returns a sorted list of strings.
// The error may be ErrSuccess in the case that no records were found.
// This is for compatibility with the old gokabinet library.
func (c *Conn) MatchPrefix(ctx context.Context, key string, maxrecords int64) ([]string, error) {
	keystransmit := []KV{
		{"prefix", []byte(key)},
		{"max", []byte(strconv.FormatInt(maxrecords, 10))},
	}

	span, ctx := opentracing.StartSpanFromContext(ctx, "ktrpc MatchPrefix")
	defer span.Finish()
	span.SetTag("prefix", key)
	span.SetTag("limit", maxrecords)

	code, m, err := c.doRPC(ctx, "/rpc/match_prefix", keystransmit)
	if err != nil {
		span.SetTag("status", err)
		return nil, err
	}
	if code != 200 {
		span.SetTag("status", code)
		return nil, makeError(m)
	}
	res := make([]string, 0, len(m))
	for _, kv := range m {
		if kv.Key[0] == '_' {
			res = append(res, string(kv.Key[1:]))
		}
	}
	if len(res) == 0 {
		span.SetTag("status", ErrSuccess)
		// yeah, gokabinet was weird here.
		return nil, ErrSuccess
	}
	return res, nil
}

var base64headers http.Header
var identityheaders http.Header

func init() {
	identityheaders = make(http.Header)
	identityheaders.Set("Content-Type", "text/tab-separated-values")
	base64headers = make(http.Header)
	base64headers.Set("Content-Type", "text/tab-separated-values; colenc=B")
}

// KV uses an explicit structure here rather than a map[string][]byte
// because we need ordered data.
type KV struct {
	Key   string
	Value []byte
}

// Do an RPC call against the KT endpoint.
func (c *Conn) doRPC(ctx context.Context, path string, values []KV) (code int, vals []KV, err error) {
	url := &url.URL{
		Scheme: c.scheme,
		Host:   c.host,
		Path:   path,
	}
	body, enc := TSVEncode(values)
	headers := identityheaders
	if enc == Base64Enc {
		headers = base64headers
	}
	resp, t, err := c.roundTrip(ctx, "POST", url, headers, body)
	if err != nil {
		return 0, nil, err
	}
	resultBody, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if !t.Stop() {
		return 0, nil, ErrTimeout
	}
	if err != nil {
		return 0, nil, err
	}
	m, err := DecodeValues(resultBody, resp.Header.Get("Content-Type"))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, m, nil
}

func (c *Conn) roundTrip(ctx context.Context, method string, url *url.URL, headers http.Header, body []byte) (*http.Response, *time.Timer, error) {
	req, t := c.makeRequest(ctx, method, url, headers, body)
	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		// Ideally we would only retry when we hit a network error. This doesn't work
		// since net/http wraps some of these errors. Do the simple thing and retry eagerly.
		t.Stop()
		c.transport.CloseIdleConnections()
		req, t = c.makeRequest(ctx, method, url, headers, body)
		resp, err = c.transport.RoundTrip(req)
		atomic.AddUint64(&c.retryCount, 1)
	}
	if err != nil {
		if !t.Stop() {
			err = ErrTimeout
		}
		return nil, nil, err
	}
	return resp, t, nil
}

func (c *Conn) makeRequest(ctx context.Context, method string, url *url.URL, headers http.Header, body []byte) (*http.Request, *time.Timer) {
	var rc io.ReadCloser
	if body != nil {
		rc = ioutil.NopCloser(bytes.NewReader(body))
	}

	// inject span context into the HTTP request header to propagate it
	// to server-side
	if span := opentracing.SpanFromContext(ctx); span != nil {
		opentracing.GlobalTracer().Inject(
			span.Context(),
			opentracing.HTTPHeaders,
			opentracing.HTTPHeadersCarrier(headers),
		)
	}

	req := &http.Request{
		Method:        method,
		URL:           url,
		Header:        headers,
		Body:          rc,
		ContentLength: int64(len(body)),
	}

	req = req.WithContext(ctx)

	t := time.AfterFunc(c.timeout, func() {
		c.transport.CancelRequest(req)
	})
	return req, t
}

type Encoding int

const (
	IdentityEnc Encoding = iota
	Base64Enc
)

// Encode the request body in TSV. The encoding is chosen based
// on whether there are any binary data in the key/values
func TSVEncode(values []KV) ([]byte, Encoding) {
	var bufsize int
	var hasbinary bool
	for _, kv := range values {
		// length of key
		hasbinary = hasbinary || hasBinary(kv.Key)
		bufsize += base64.StdEncoding.EncodedLen(len(kv.Key))
		// tab
		bufsize += 1
		// value
		hasbinary = hasbinary || hasBinarySlice(kv.Value)
		bufsize += base64.StdEncoding.EncodedLen(len(kv.Value))
		// newline
		bufsize += 1
	}
	buf := make([]byte, bufsize)
	var n int
	for _, kv := range values {
		if hasbinary {
			base64.StdEncoding.Encode(buf[n:], []byte(kv.Key))
			n += base64.StdEncoding.EncodedLen(len(kv.Key))
		} else {
			n += copy(buf[n:], kv.Key)
		}
		buf[n] = '\t'
		n++
		if hasbinary {
			base64.StdEncoding.Encode(buf[n:], kv.Value)
			n += base64.StdEncoding.EncodedLen(len(kv.Value))
		} else {
			n += copy(buf[n:], kv.Value)
		}
		buf[n] = '\n'
		n++
	}
	enc := IdentityEnc
	if hasbinary {
		enc = Base64Enc
	}
	return buf, enc
}

func hasBinary(b string) bool {
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < 0x20 || c > 0x7e {
			return true
		}
	}
	return false
}

func hasBinarySlice(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return true
		}
	}
	return false
}

// DecodeValues takes a response from an KT RPC call decodes it into a list of key
// value pairs.
func DecodeValues(buf []byte, contenttype string) ([]KV, error) {
	if len(buf) == 0 {
		return nil, nil
	}
	// Ideally, we should parse the mime media type here,
	// but this is an expensive operation because mime is just
	// that awful.
	//
	// KT can return values in 3 different formats, Tab separated values (TSV) without any field encoding,
	// TSV with fields base64 encoded or TSV with URL encoding.
	// KT does not give you any option as to the format that it returns, so we have to implement all of them
	//
	// KT responses are pretty simple and we can rely
	// on it putting the parameter of colenc=[BU] at
	// the end of the string. Just look for B, U or s
	// (last character of tab-separated-values)
	// to figure out which field encoding is used.
	var decodef decodefunc
	switch contenttype[len(contenttype)-1] {
	case 'B':
		decodef = base64Decode
	case 'U':
		decodef = urlDecode
	case 's':
		decodef = identityDecode
	default:
		return nil, &Error{Message: fmt.Sprintf("responded with unknown Content-Type: %s", contenttype)}
	}

	// Because of the encoding, we can tell how many records there
	// are by scanning through the input and counting the \n's
	var recCount int
	for _, v := range buf {
		if v == '\n' {
			recCount++
		}
	}
	result := make([]KV, 0, recCount)
	b := bytes.NewBuffer(buf)
	for {
		key, err := b.ReadBytes('\t')
		if err != nil {
			return result, nil
		}
		key = decodef(key[:len(key)-1])
		value, err := b.ReadBytes('\n')
		if len(value) > 0 {
			fieldlen := len(value) - 1
			if value[len(value)-1] != '\n' {
				fieldlen = len(value)
			}
			value = decodef(value[:fieldlen])
			result = append(result, KV{string(key), value})
		}
		if err != nil {
			return result, nil
		}
	}
}

// decodefunc takes a byte slice and decodes the
// value in place. It returns a slice pointing into
// the original byte slice. It is used for decoding the
// individual fields of the TSV that kt returns
type decodefunc func([]byte) []byte

// Don't do anything, this is pure TSV
func identityDecode(b []byte) []byte {
	return b
}

// Base64 decode each of the field
func base64Decode(b []byte) []byte {
	n, _ := base64.StdEncoding.Decode(b, b)
	return b[:n]
}

// Decode % escaped URL format
func urlDecode(b []byte) []byte {
	res := b
	resi := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '%' {
			res[resi] = b[i]
			resi++
			continue
		}
		res[resi] = unhex(b[i+1])<<4 | unhex(b[i+2])
		resi++
		i += 2
	}
	return res[:resi]
}

// copied from net/url
func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}

// TODO: make this return errors that can be introspected more easily
// and make it trim components of the error to filter out unused information.
func makeError(m []KV) error {
	kv := findRec(m, "ERROR")
	if kv.Key == "" {
		return &Error{Message: "generic error"}
	}
	return &Error{Message: string(kv.Value)}
}

func findRec(kvs []KV, key string) KV {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv
		}
	}
	return KV{}
}

// empty header for REST calls.
var emptyHeader = make(http.Header)

func (c *Conn) doREST(ctx context.Context, op string, key string, val []byte) (code int, body []byte, err error) {
	newkey := urlenc(key)
	url := &url.URL{
		Scheme: c.scheme,
		Host:   c.host,
		Opaque: newkey,
	}
	resp, t, err := c.roundTrip(ctx, op, url, emptyHeader, val)
	if err != nil {
		return 0, nil, err
	}
	resultBody, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if !t.Stop() {
		err = ErrTimeout
	}
	return resp.StatusCode, resultBody, err
}

// encode the key for use in a RESTFUL url
// KT requires that we use URL escaped values for
// anything not safe in a query component.
// Add a slash for the leading slash in the url.
func urlenc(s string) string {
	return "/" + url.QueryEscape(s)
}
