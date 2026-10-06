package otel

import (
	"bytes"
	"encoding/base64"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

var (
	httpProtocolNameAttr = semconv.NetworkProtocolName("http")
	http11VersionAttr    = semconv.NetworkProtocolVersion("1.1")
	http10VersionAttr    = semconv.NetworkProtocolVersion("1.0")
	enduserIDKey         = attribute.Key("enduser.id")
)

// maxIPLength is the longest address appendIP writes, an IPv4-mapped IPv6 one.
const maxIPLength = len("ffff:ffff:ffff:ffff:ffff:ffff:255.255.255.255")

// querySlack is room for redacted values shorter than "REDACTED".
const querySlack = 16

// startAttributes returns the attributes known before the handler runs. Request values are
// copied into one buffer, since fasthttp reuses its own once the handler returns.
func (m *middleware) startAttributes(c fiber.Ctx, r *request, settings *appSettings) []attribute.KeyValue {
	request := c.Request()
	uri := request.URI()
	host := c.Hostname()
	path := uri.Path()
	rawQuery := uri.QueryString()
	rawUserAgent := request.Header.UserAgent()

	var (
		transport attribute.KeyValue
		peerIP    net.IP
		peerPort  int
	)
	switch addr := c.RequestCtx().RemoteAddr().(type) {
	case *net.TCPAddr:
		transport = semconv.NetworkTransportTCP
		peerIP, peerPort = addr.IP, addr.Port
	case *net.UnixAddr:
		transport = semconv.NetworkTransportUnix
	}

	size := len(host) + len(path) + len(rawUserAgent)
	if len(rawQuery) > 0 {
		size += len(rawQuery) + querySlack
	}

	// The client is the peer unless a trusted proxy reports it.
	var proxiedIP string
	proxied := false
	if m.clientIP {
		size += maxIPLength
		if proxied = c.IsProxyTrusted(); proxied {
			proxiedIP = c.IP()
			size += len(proxiedIP)
		}
	}

	buf := make([]byte, 0, size)
	buf = append(buf, host...)
	hostEnd := len(buf)
	buf = append(buf, path...)
	pathEnd := len(buf)
	buf = appendRedactedQuery(buf, rawQuery, m.RedactedQueryParams)
	queryEnd := len(buf)
	buf = append(buf, rawUserAgent...)
	userAgentEnd := len(buf)
	if m.clientIP && peerIP != nil {
		buf = appendIP(buf, peerIP)
	}
	peerEnd := len(buf)
	buf = append(buf, proxiedIP...)
	values := utils.UnsafeString(buf)

	query, userAgent, peer, client := values[pathEnd:queryEnd], values[queryEnd:userAgentEnd], values[userAgentEnd:peerEnd], values[peerEnd:]
	if !proxied {
		client = peer
	}

	// server.port is the configured port, or else the one the Host header names.
	serverPort := m.serverPortAttr
	if m.Port == nil {
		if port, ok := hostPort(c.Host()); ok {
			serverPort = semconv.ServerPort(port)
		}
	}
	username, hasUsername := HasBasicAuth(c.Get(fiber.HeaderAuthorization))

	var custom []attribute.KeyValue
	if m.CustomAttributes != nil {
		custom = m.CustomAttributes(c)
	}

	n := 6 + len(m.requestHeaders) + len(custom)
	for _, present := range [...]bool{
		transport.Valid(), serverPort.Valid(), query != "", userAgent != "", peer != "",
		peer != "" && peerPort > 0, client != "", r.requestSizeKnown, hasUsername,
	} {
		if present {
			n++
		}
	}

	attrs := make([]attribute.KeyValue, 0, n)
	attrs = append(attrs,
		semconv.HTTPRequestMethodKey.String(c.Method()),
		semconv.URLScheme(c.Scheme()),
		semconv.URLPath(values[hostEnd:pathEnd]),
		semconv.ServerAddress(values[:hostEnd]),
		httpProtocolNameAttr,
		httpProtocolVersionAttr(c),
	)
	if transport.Valid() {
		attrs = append(attrs, transport)
	}
	if serverPort.Valid() {
		attrs = append(attrs, serverPort)
	}
	if query != "" {
		attrs = append(attrs, semconv.URLQuery(query))
	}
	if userAgent != "" {
		attrs = append(attrs, semconv.UserAgentOriginal(userAgent))
	}
	if peer != "" {
		attrs = append(attrs, semconv.NetworkPeerAddress(peer))
		if peerPort > 0 {
			attrs = append(attrs, semconv.NetworkPeerPort(peerPort))
		}
	}
	if client != "" {
		attrs = append(attrs, semconv.ClientAddress(client))
	}
	if r.requestSizeKnown {
		attrs = append(attrs, semconv.HTTPRequestBodySize(int(r.requestSize)))
	}
	if hasUsername {
		attrs = append(attrs, enduserIDKey.String(username))
	}

	for _, header := range m.requestHeaders {
		if headerValues := requestHeaderValues(&request.Header, header.name, settings.disableHeaderNormalizing); len(headerValues) > 0 {
			attrs = append(attrs, header.key.StringSlice(headerValues))
		}
	}

	return append(attrs, custom...)
}

// hostPort returns the port a Host header value names, if it names one.
func hostPort(host string) (int, bool) {
	_, port, ok := utils.SplitHostPort(utils.TrimSpace(host))
	if !ok {
		return 0, false
	}

	parsed, err := utils.ParseUint16(port)
	if err != nil || parsed == 0 {
		return 0, false
	}

	return int(parsed), true
}

type capturedHeader struct {
	name string
	key  attribute.Key
}

// capturedHeaders maps header names to attribute keys, dropping blanks and duplicates.
func capturedHeaders(prefix string, names []string) []capturedHeader {
	headers := make([]capturedHeader, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		key := attribute.Key(prefix + strings.ToLower(name))
		if slices.ContainsFunc(headers, func(header capturedHeader) bool { return header.key == key }) {
			continue
		}
		headers = append(headers, capturedHeader{name: name, key: key})
	}

	return headers
}

// requestHeaderValues copies the header's values; folding matches names case-insensitively.
func requestHeaderValues(header *fasthttp.RequestHeader, name string, folding bool) []string {
	if !folding {
		return copyValues(header.PeekAll(name))
	}

	var values []string
	for key, value := range header.All() {
		if utils.EqualFold(utils.UnsafeString(key), name) {
			values = append(values, string(value))
		}
	}

	return values
}

func responseHeaderValues(header *fasthttp.ResponseHeader, name string, folding bool) []string {
	if !folding {
		return copyValues(header.PeekAll(name))
	}

	var values []string
	for key, value := range header.All() {
		if utils.EqualFold(utils.UnsafeString(key), name) {
			values = append(values, string(value))
		}
	}

	return values
}

func httpProtocolVersionAttr(c fiber.Ctx) attribute.KeyValue {
	if c.Request().Header.IsHTTP11() {
		return http11VersionAttr
	}

	return http10VersionAttr
}

// appendIP appends ip as net.IP.String formats it.
func appendIP(dst []byte, ip net.IP) []byte {
	if addr, ok := netip.AddrFromSlice(ip); ok {
		return addr.Unmap().AppendTo(dst)
	}

	return append(dst, ip.String()...)
}

// requestBodySize returns the request body size and whether it is known.
func requestBodySize(c fiber.Ctx) (int64, bool) {
	request := c.Request()
	switch {
	case !c.HasBody():
		return 0, true
	case request.IsBodyStream():
		// Content-Length may over-declare a stream fasthttp has only partly read.
		return bodyStreamSize(request.BodyStream())
	}

	// Content-Length avoids re-marshaling a parsed multipart body into memory.
	if contentLength := request.Header.ContentLength(); contentLength > 0 {
		return int64(contentLength), true
	}

	return int64(len(request.Body())), true
}

// responseBodySize returns the response body size and whether it is known.
func responseBodySize(c fiber.Ctx) (int64, bool) {
	if responseBodySuppressed(c) {
		return 0, true
	}

	response := c.Response()
	if !response.IsBodyStream() {
		return int64(len(response.Body())), true
	}

	if contentLength := response.Header.ContentLength(); contentLength >= 0 {
		return int64(contentLength), true
	}

	// Measuring a chunked stream would mean replacing it, which truncates it (#1734).
	return bodyStreamSize(response.BodyStream())
}

// bodyStreamSize reports stream's exact remaining length, without reading it.
func bodyStreamSize(stream io.Reader) (int64, bool) {
	switch reader := stream.(type) {
	case *io.LimitedReader:
		if reader.N >= 0 {
			return reader.N, true
		}
	case *bytes.Reader:
		return int64(reader.Len()), true
	case *bytes.Buffer:
		return int64(reader.Len()), true
	case *strings.Reader:
		return int64(reader.Len()), true
	}

	return 0, false
}

// responseBodySuppressed mirrors fasthttp's mustSkipBody: only headers are sent.
func responseBodySuppressed(c fiber.Ctx) bool {
	if c.Method() == fiber.MethodHead || c.Response().SkipBody {
		return true
	}

	// 1xx, 204 and 304 responses must not include a message body.
	status := c.Response().StatusCode()

	return (status >= 100 && status < 200) || status == fiber.StatusNoContent || status == fiber.StatusNotModified
}

// serverErrorTypes are the precomputed error.type attributes of 5xx statuses.
var serverErrorTypes = func() (types [100]attribute.KeyValue) {
	for i := range types {
		types[i] = semconv.ErrorTypeKey.String(strconv.Itoa(fiber.StatusInternalServerError + i))
	}

	return types
}()

func statusErrorType(status int) attribute.KeyValue {
	if status >= fiber.StatusInternalServerError && status < fiber.StatusInternalServerError+len(serverErrorTypes) {
		return serverErrorTypes[status-fiber.StatusInternalServerError]
	}

	return semconv.ErrorTypeKey.String(strconv.Itoa(status))
}

// routePattern returns the matched route's pattern, or "" when no route matched.
func routePattern(c fiber.Ctx) string {
	if !c.Matched() {
		return ""
	}

	// A registered route always carries a handler; the synthetic one does not.
	route := c.Route()
	if route == nil || len(route.Handlers) == 0 {
		return ""
	}

	return route.Path
}

// sensitiveQueryParams are the parameters semconv redacts from url.query.
var sensitiveQueryParams = [...]string{
	"AWSAccessKeyId",
	"Signature",
	"sig",
	"X-Goog-Signature",
	"X-Amz-Signature",
	"X-Amz-Credential",
	"X-Amz-Security-Token",
}

const redactedQueryValue = "REDACTED"

// appendRedactedQuery appends query with sensitive and extra parameter values redacted.
func appendRedactedQuery(dst, query []byte, extra []string) []byte {
	if len(query) == 0 {
		return dst
	}

	for {
		pair, rest, more := bytes.Cut(query, []byte{'&'})
		if key, value, ok := bytes.Cut(pair, []byte{'='}); ok && len(value) > 0 && isSensitiveQueryParam(key, extra) {
			dst = append(dst, key...)
			dst = append(dst, '=')
			dst = append(dst, redactedQueryValue...)
		} else {
			dst = append(dst, pair...)
		}

		if !more {
			return dst
		}
		dst = append(dst, '&')
		query = rest
	}
}

// isSensitiveQueryParam matches case-sensitively, as semconv specifies.
func isSensitiveQueryParam(key []byte, extra []string) bool {
	for _, sensitive := range sensitiveQueryParams {
		if string(key) == sensitive {
			return true
		}
	}

	return slices.Contains(extra, string(key))
}

// HasBasicAuth returns the username of a Basic Authorization header value, if it is one.
func HasBasicAuth(auth string) (string, bool) {
	if auth == "" {
		return "", false
	}

	// Check if the Authorization header is Basic.
	// Auth schemes are case-insensitive.
	if len(auth) < 6 || !utils.EqualFold(auth[:6], "Basic ") {
		return "", false
	}

	// Decode the header contents
	raw, err := base64.StdEncoding.DecodeString(auth[6:])
	if err != nil {
		return "", false
	}

	// Check if the decoded credentials are in the correct form
	// which is "username:password".
	index := bytes.IndexByte(raw, ':')
	if index == -1 {
		return "", false
	}

	// Get the username
	return string(raw[:index]), true
}
