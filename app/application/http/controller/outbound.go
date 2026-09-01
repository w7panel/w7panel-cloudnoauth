package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/w7panel/w7panel-cloudnoauth/app/application/logic"
	"github.com/w7panel/w7panel-cloudnoauth/common/helper"
	"github.com/w7panel/w7panel-cloudnoauth/common/service/k8s"
	"github.com/we7coreteam/w7-rangine-go/v2/src/http/controller"
)

const defaultAllowedOutboundHost = "api.w7.cc"

const (
	outboundMaxIdleConns        = 200
	outboundMaxIdleConnsPerHost = 100
	outboundMaxConnsPerHost     = 200
	outboundDialTimeout         = 30 * time.Second
)

func newOutboundTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = outboundMaxIdleConns
	transport.MaxIdleConnsPerHost = outboundMaxIdleConnsPerHost
	transport.MaxConnsPerHost = outboundMaxConnsPerHost
	return transport
}

func newUpstreamOutboundTransport(allowedHosts []string, upstreamHost, upstreamCAFile string) (*http.Transport, error) {
	upstreamHost = strings.TrimSuffix(strings.TrimSpace(upstreamHost), ".")
	if upstreamHost == "" {
		return nil, fmt.Errorf("upstream host is required")
	}
	if strings.ContainsAny(upstreamHost, "/:") {
		return nil, fmt.Errorf("upstream host %q must be a DNS name without scheme or port", upstreamHost)
	}

	transport := newOutboundTransport()
	transport.Proxy = nil

	if strings.TrimSpace(upstreamCAFile) != "" {
		certificateData, err := os.ReadFile(upstreamCAFile)
		if err != nil {
			return nil, fmt.Errorf("read upstream CA file: %w", err)
		}
		certificatePool := x509.NewCertPool()
		if !certificatePool.AppendCertsFromPEM(certificateData) {
			return nil, fmt.Errorf("upstream CA file %q contains no certificates", upstreamCAFile)
		}
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    certificatePool,
		}
	}

	dialer := &net.Dialer{
		Timeout:   outboundDialTimeout,
		KeepAlive: 30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		upstreamAddress, replaced := replaceUpstreamDialAddress(address, allowedHosts, upstreamHost)
		if !replaced {
			return dialer.DialContext(ctx, network, address)
		}
		return dialer.DialContext(ctx, network, upstreamAddress)
	}
	return transport, nil
}

func replaceUpstreamDialAddress(address string, allowedHosts []string, upstreamHost string) (string, bool) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !helper.IsAllowedHost(host, allowedHosts) {
		return address, false
	}
	return net.JoinHostPort(upstreamHost, port), true
}

type Outbound struct {
	controller.Abstract
	CredentialLogic *logic.Credential
	Scheme          string
	AllowedHosts    []string
	reverseProxy    *httputil.ReverseProxy
}

func NewOutbound(credentialLogic *logic.Credential, scheme string, allowedHost string) Outbound {
	return newOutbound(credentialLogic, scheme, allowedHost, newOutboundTransport())
}

func NewOutboundWithUpstream(credentialLogic *logic.Credential, scheme, allowedHost, upstreamHost, upstreamCAFile string) (Outbound, error) {
	allowedHosts := helper.ParseAllowedHosts(allowedHost, defaultAllowedOutboundHost)
	transport, err := newUpstreamOutboundTransport(allowedHosts, upstreamHost, upstreamCAFile)
	if err != nil {
		return Outbound{}, err
	}
	return newOutbound(credentialLogic, scheme, allowedHost, transport), nil
}

func newOutbound(credentialLogic *logic.Credential, scheme string, allowedHost string, transport http.RoundTripper) Outbound {
	if strings.TrimSpace(scheme) == "" {
		scheme = "https"
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
	}
	proxy.Director = func(req *http.Request) {
		originalHost := req.Host
		req.URL.Scheme = scheme
		req.URL.Host = originalHost
		req.Host = originalHost
		req.Header.Set("X-Forwarded-Host", originalHost)
		req.Header.Set("X-Appid-Proxy", "w7panel-appid-proxy")
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		args := []any{
			"method", resp.Request.Method,
			"url", resp.Request.URL.String(),
			"status", resp.StatusCode,
			"host", resp.Request.Host,
		}
		slog.Info("outbound upstream response", args...)
		return nil
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, req *http.Request, err error) {
		args := []any{
			"method", req.Method,
			"url", req.URL.String(),
			"host", req.Host,
			"error", err,
		}
		slog.Error("outbound upstream error", args...)
		http.Error(writer, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	}

	return Outbound{
		CredentialLogic: credentialLogic,
		Scheme:          scheme,
		AllowedHosts:    helper.ParseAllowedHosts(allowedHost, defaultAllowedOutboundHost),
		reverseProxy:    proxy,
	}
}

func (c Outbound) Forward(ctx *gin.Context) {
	slog.Info("outbound request received",
		"method", ctx.Request.Method,
		"path", ctx.Request.URL.Path,
		"query", ctx.Request.URL.RawQuery,
		"host", ctx.Request.Host,
		"remote_addr", ctx.Request.RemoteAddr,
		"content_length", ctx.Request.ContentLength,
		"content_type", ctx.Request.Header.Get("Content-Type"),
	)

	if !helper.IsAllowedHost(ctx.Request.Host, c.AllowedHosts) {
		slog.Warn("outbound host rejected",
			"host", ctx.Request.Host,
			"allowed_hosts", c.AllowedHosts,
		)
		ctx.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"message": "host not allowed",
		})
		return
	}

	if err := appendSignedBody(ctx.Request, func() (k8s.AppCredential, error) {
		return c.CredentialLogic.Resolve(ctx.Request.Context())
	}); err != nil {
		slog.Warn("outbound append signed body failed",
			"path", ctx.Request.URL.Path,
			"error", err,
		)
	}

	slog.Info("outbound forwarding request",
		"method", ctx.Request.Method,
		"host", ctx.Request.Host,
		"path", ctx.Request.URL.Path,
		"content_length", ctx.Request.ContentLength,
	)
	c.reverseProxy.ServeHTTP(ctx.Writer, ctx.Request)
}

func appendSignedBody(req *http.Request, resolveCredential func() (k8s.AppCredential, error)) error {
	if req.URL != nil && req.URL.Path == "/" {
		return nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	_ = req.Body.Close()

	contentType := req.Header.Get("Content-Type")
	normalizedContentType := strings.ToLower(strings.TrimSpace(contentType))
	mediaType, _, mediaTypeErr := mime.ParseMediaType(contentType)
	if mediaTypeErr == nil && mediaType == "multipart/form-data" {
		return appendSignedMultipartFormBody(req, body, contentType, resolveCredential)
	}
	if mediaTypeErr != nil && strings.HasPrefix(normalizedContentType, "multipart/form-data") {
		return mediaTypeErr
	}
	if strings.Contains(normalizedContentType, "application/x-www-form-urlencoded") {
		return appendSignedFormBody(req, body, contentType, resolveCredential)
	}

	return appendSignedJSONBody(req, body, contentType, resolveCredential)
}

func appendSignedMultipartFormBody(req *http.Request, body []byte, contentType string, resolveCredential func() (k8s.AppCredential, error)) error {
	data, err := helper.ParseMultipartFormBody(body, contentType)
	if err != nil {
		return err
	}
	if _, exists := data["sign"]; exists {
		resetRequestBody(req, contentType, body)
		return nil
	}

	credential, err := resolveCredential()
	if err != nil {
		if k8s.IsSkippableCredentialError(err) {
			resetRequestBody(req, contentType, body)
			return nil
		}
		return err
	}

	nonce, err := helper.RandomString(16)
	if err != nil {
		return err
	}

	data["appid"] = credential.AppID
	data["timestamp"] = time.Now().Unix()
	data["nonce"] = nonce
	signature := helper.BuildSign(data, credential.AppSecret)
	data["sign"] = signature

	fields := map[string]string{
		"appid":     credential.AppID,
		"timestamp": fmt.Sprint(data["timestamp"]),
		"nonce":     nonce,
		"sign":      signature,
	}
	encodedBody, err := helper.AppendMultipartFormFields(body, contentType, fields)
	if err != nil {
		return err
	}
	resetRequestBody(req, contentType, encodedBody)
	return nil
}

func appendSignedFormBody(req *http.Request, body []byte, contentType string, resolveCredential func() (k8s.AppCredential, error)) error {
	data, err := helper.ParsePHPFormBody(body)
	if err != nil {
		return err
	}
	if _, exists := data["sign"]; exists {
		resetRequestBody(req, contentType, body)
		return nil
	}

	credential, err := resolveCredential()
	if err != nil {
		if k8s.IsSkippableCredentialError(err) {
			resetRequestBody(req, contentType, body)
			return nil
		}
		return err
	}

	nonce, err := helper.RandomString(16)
	if err != nil {
		return err
	}

	data["appid"] = credential.AppID
	data["timestamp"] = time.Now().Unix()
	data["nonce"] = nonce
	data["sign"] = helper.BuildSign(data, credential.AppSecret)

	encodedBody := helper.EncodePHPQuery(data)
	resetRequestBody(req, contentType, []byte(encodedBody))
	return nil
}

func appendSignedJSONBody(req *http.Request, body []byte, contentType string, resolveCredential func() (k8s.AppCredential, error)) error {
	data := map[string]any{}
	if len(bytes.TrimSpace(body)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&data); err != nil {
			return err
		}
	}
	if _, exists := data["sign"]; exists {
		resetRequestBody(req, contentType, body)
		return nil
	}

	credential, err := resolveCredential()
	if err != nil {
		if k8s.IsSkippableCredentialError(err) {
			resetRequestBody(req, contentType, body)
			return nil
		}
		return err
	}

	nonce, err := helper.RandomString(16)
	if err != nil {
		return err
	}

	data["appid"] = credential.AppID
	data["timestamp"] = time.Now().Unix()
	data["nonce"] = nonce
	data["sign"] = helper.BuildSign(data, credential.AppSecret)

	encodedBody, err := json.Marshal(data)
	if err != nil {
		return err
	}
	resetRequestBody(req, contentType, encodedBody)
	return nil
}

func resetRequestBody(req *http.Request, contentType string, body []byte) {
	resetRawRequestBody(req, body)
	req.Header.Set("Content-Type", contentType)
}

func resetRawRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
}
