package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

type Proxy struct {
	controller.Abstract
	CredentialLogic *logic.Credential
	Scheme          string
	reverseProxy    *httputil.ReverseProxy
}

func NewProxy(credentialLogic *logic.Credential, scheme string) Proxy {
	if strings.TrimSpace(scheme) == "" {
		scheme = "https"
	}
	proxy := &httputil.ReverseProxy{}
	proxy.Director = func(req *http.Request) {
		originalHost := req.Host
		req.URL.Scheme = scheme
		req.URL.Host = originalHost
		req.Host = originalHost
		req.Header.Set("X-Forwarded-Host", originalHost)
		req.Header.Set("X-Appid-Proxy", "w7panel-appid-proxy")
	}

	return Proxy{
		CredentialLogic: credentialLogic,
		Scheme:          scheme,
		reverseProxy:    proxy,
	}
}

func (c Proxy) Live(ctx *gin.Context) {
	c.JsonResponseWithoutError(ctx, gin.H{
		"message": "ok",
	})
}

func (c Proxy) Credential(ctx *gin.Context) {
	credential, err := c.CredentialLogic.ResolveByRemoteIP(
		ctx.Request.Context(),
		ctx.ClientIP(),
	)
	if err != nil {
		c.JsonResponseWithServerError(ctx, err)
		return
	}

	c.JsonResponseWithoutError(ctx, gin.H{
		"appid":     credential.AppID,
		"appsecret": credential.AppSecret,
	})
}

func (c Proxy) Proxy(ctx *gin.Context) {
	if err := appendSignedBody(ctx.Request, func() (k8s.AppCredential, error) {
		return c.CredentialLogic.ResolveByRemoteIP(
			ctx.Request.Context(),
			ctx.ClientIP(),
		)
	}); err != nil {
		c.JsonResponseWithServerError(ctx, err)
		return
	}

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
		return appendSignedFormBody(req, body, resolveCredential)
	}

	return appendSignedJSONBody(req, body, resolveCredential)
}

func appendSignedFormBody(req *http.Request, body []byte, resolveCredential func() (k8s.AppCredential, error)) error {
	data, err := helper.ParsePHPFormBody(body)
	if err != nil {
		return err
	}
	if _, exists := data["sign"]; exists {
		resetRequestBody(req, "application/x-www-form-urlencoded", body)
		return nil
	}

	credential, err := resolveCredential()
	if err != nil {
		if isSkippableCredentialError(err) {
			resetRequestBody(req, "application/x-www-form-urlencoded", body)
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
	resetRequestBody(req, "application/x-www-form-urlencoded", []byte(encodedBody))
	return nil
}

func appendSignedJSONBody(req *http.Request, body []byte, resolveCredential func() (k8s.AppCredential, error)) error {
	data := map[string]any{}
	if len(bytes.TrimSpace(body)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&data); err != nil {
			return err
		}
	}
	if _, exists := data["sign"]; exists {
		resetRequestBody(req, "application/json", body)
		return nil
	}

	credential, err := resolveCredential()
	if err != nil {
		if isSkippableCredentialError(err) {
			resetRequestBody(req, "application/json", body)
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
	resetRequestBody(req, "application/json", encodedBody)
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

func isSkippableCredentialError(err error) bool {
	return k8s.IsSkippableCredentialError(err)
}
