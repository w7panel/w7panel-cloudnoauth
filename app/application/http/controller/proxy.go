package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/w7panel/w7panel-appid-proxy/app/application/logic"
	"github.com/w7panel/w7panel-appid-proxy/common/helper"
	"github.com/w7panel/w7panel-appid-proxy/common/service/k8s"
	"github.com/we7coreteam/w7-rangine-go/v2/src/http/controller"
)

const defaultAllowedProxyHost = "api.w7.cc"

const (
	proxyMaxIdleConns        = 200
	proxyMaxIdleConnsPerHost = 100
	proxyMaxConnsPerHost     = 200
)

func newProxyTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = proxyMaxIdleConns
	transport.MaxIdleConnsPerHost = proxyMaxIdleConnsPerHost
	transport.MaxConnsPerHost = proxyMaxConnsPerHost
	return transport
}

type Proxy struct {
	controller.Abstract
	CredentialLogic *logic.Credential
	Scheme          string
	AllowedHosts    []string
	reverseProxy    *httputil.ReverseProxy
}

func NewProxy(credentialLogic *logic.Credential, scheme string, allowedHost string) Proxy {
	if strings.TrimSpace(scheme) == "" {
		scheme = "https"
	}
	proxy := &httputil.ReverseProxy{
		Transport: newProxyTransport(),
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
		slog.Info("proxy upstream response", args...)
		return nil
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, req *http.Request, err error) {
		args := []any{
			"method", req.Method,
			"url", req.URL.String(),
			"host", req.Host,
			"error", err,
		}
		slog.Error("proxy upstream error", args...)
		http.Error(writer, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	}

	return Proxy{
		CredentialLogic: credentialLogic,
		Scheme:          scheme,
		AllowedHosts:    helper.ParseAllowedHosts(allowedHost, defaultAllowedProxyHost),
		reverseProxy:    proxy,
	}
}

func (c Proxy) Live(ctx *gin.Context) {
	c.JsonResponseWithoutError(ctx, gin.H{
		"message": "ok",
	})
}

func (c Proxy) Credential(ctx *gin.Context) {
	remoteIP, err := helper.RemoteIPFromRequest(ctx.Request)
	if err != nil {
		slog.Warn("credential remote ip parse failed",
			"remote_addr", ctx.Request.RemoteAddr,
			"error", err,
		)
		c.JsonResponseWithServerError(ctx, err)
		return
	}

	slog.Info("credential resolve requested",
		"remote_ip", remoteIP,
		"path", ctx.Request.URL.Path,
	)
	credential, err := c.CredentialLogic.ResolveByRemoteIP(
		ctx.Request.Context(),
		remoteIP,
	)
	if err != nil {
		slog.Warn("credential resolve failed",
			"remote_ip", remoteIP,
			"error", err,
		)
		c.JsonResponseWithServerError(ctx, err)
		return
	}
	slog.Info("credential resolve succeeded",
		"remote_ip", remoteIP,
		"pod", credential.PodName,
		"appgroup", credential.AppGroup,
		"appid", credential.AppID,
	)

	c.JsonResponseWithoutError(ctx, gin.H{
		"appid":     credential.AppID,
		"appsecret": credential.AppSecret,
	})
}

func (c Proxy) Proxy(ctx *gin.Context) {
	slog.Info("proxy request received",
		"method", ctx.Request.Method,
		"path", ctx.Request.URL.Path,
		"query", ctx.Request.URL.RawQuery,
		"host", ctx.Request.Host,
		"remote_addr", ctx.Request.RemoteAddr,
		"content_length", ctx.Request.ContentLength,
		"content_type", ctx.Request.Header.Get("Content-Type"),
	)

	if !helper.IsAllowedHost(ctx.Request.Host, c.AllowedHosts) {
		slog.Warn("proxy host rejected",
			"host", ctx.Request.Host,
			"allowed_hosts", c.AllowedHosts,
		)
		ctx.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"message": "host not allowed",
		})
		return
	}

	remoteIP, err := helper.RemoteIPFromRequest(ctx.Request)
	if err != nil {
		slog.Warn("proxy remote ip parse failed",
			"remote_addr", ctx.Request.RemoteAddr,
			"error", err,
		)
		c.JsonResponseWithServerError(ctx, err)
		return
	}

	if err := appendSignedBody(ctx.Request, func() (k8s.AppCredential, error) {
		return c.CredentialLogic.ResolveByRemoteIP(
			ctx.Request.Context(),
			remoteIP,
		)
	}); err != nil {
		slog.Warn("proxy append signed body failed",
			"remote_ip", remoteIP,
			"path", ctx.Request.URL.Path,
			"error", err,
		)
		c.JsonResponseWithServerError(ctx, err)
		return
	}

	slog.Info("proxy forwarding request",
		"remote_ip", remoteIP,
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
	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		return appendSignedFormBody(req, body, contentType, resolveCredential)
	}

	return appendSignedJSONBody(req, body, contentType, resolveCredential)
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
