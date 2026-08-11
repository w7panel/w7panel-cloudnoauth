package controller

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/fcgi"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/w7panel/w7panel-cloudnoauth/app/application/logic"
	"github.com/w7panel/w7panel-cloudnoauth/common/helper"
	"github.com/w7panel/w7panel-cloudnoauth/common/service/k8s"
)

var signatureFields = []string{"appid", "timestamp", "nonce", "sign"}

const (
	requestSourceHeader = "X-Request-Source"
	apiRequestSource    = "api.w7.cc"
)

type Inbound struct {
	CredentialLogic *logic.Credential
	reverseProxy    *httputil.ReverseProxy
	fastCGIProxy    *fastCGIProxy
}

func NewInbound(credentialLogic *logic.Credential, targetScheme, targetHost string) (*Inbound, error) {
	target, err := url.Parse(targetScheme + "://" + targetHost)
	if err != nil {
		return nil, fmt.Errorf("parse inbound target: %w", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(writer http.ResponseWriter, req *http.Request, err error) {
		slog.Error("inbound proxy error", "path", req.URL.Path, "error", err)
		http.Error(writer, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
	}
	return &Inbound{
		CredentialLogic: credentialLogic,
		reverseProxy:    proxy,
		fastCGIProxy:    newFastCGIProxy(targetHost),
	}, nil
}

func (h *Inbound) ServeHTTP(writer http.ResponseWriter, req *http.Request) {
	slog.Info("inbound request", "path", req.URL.Path)
	if requiresInboundSignature(req) {
		err := verifyAndStripSignedBody(req, func() (k8s.AppCredential, error) {
			return h.CredentialLogic.Resolve(req.Context())
		})
		if err != nil {
			slog.Warn("inbound signature rejected", "path", req.URL.Path, "error", err)
			http.Error(writer, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
	}
	if fcgi.ProcessEnv(req) != nil {
		h.fastCGIProxy.ServeHTTP(writer, req)
		return
	}
	h.reverseProxy.ServeHTTP(writer, req)
}

// requiresInboundSignature recognizes the platform API request marker. Only a
// successfully verified request may reach the application with this header.
func requiresInboundSignature(req *http.Request) bool {
	source := strings.TrimSpace(req.Header.Get(requestSourceHeader))
	if strings.EqualFold(source, apiRequestSource) {
		req.Header.Set(requestSourceHeader, apiRequestSource)
		return true
	}
	req.Header.Del(requestSourceHeader)
	return false
}

func verifyAndStripSignedBody(req *http.Request, resolveCredential func() (k8s.AppCredential, error)) error {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	_ = req.Body.Close()

	contentType := req.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		data, err := helper.ParsePHPFormBody(body)
		if err != nil {
			return err
		}
		encoded, changed, err := verifyAndStripSignature(data, resolveCredential)
		if err != nil {
			return err
		}
		if changed {
			resetRequestBody(req, contentType, []byte(helper.EncodePHPQuery(encoded)))
		} else {
			resetRequestBody(req, contentType, body)
		}
		return nil
	}

	data := map[string]any{}
	if len(bytes.TrimSpace(body)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&data); err != nil {
			return err
		}
	}
	stripped, changed, err := verifyAndStripSignature(data, resolveCredential)
	if err != nil {
		return err
	}
	if !changed {
		resetRequestBody(req, contentType, body)
		return nil
	}
	encoded, err := json.Marshal(stripped)
	if err != nil {
		return err
	}
	resetRequestBody(req, contentType, encoded)
	return nil
}

func verifyAndStripSignature(data map[string]any, resolveCredential func() (k8s.AppCredential, error)) (map[string]any, bool, error) {
	signValue, signed := data["sign"]
	if !signed {
		return nil, false, fmt.Errorf("signature is required")
	}
	signature, ok := signValue.(string)
	if !ok || signature == "" {
		return nil, false, fmt.Errorf("invalid signature value")
	}

	credential, err := resolveCredential()
	if err != nil {
		return nil, false, err
	}
	if fmt.Sprint(data["appid"]) != credential.AppID {
		return nil, false, fmt.Errorf("appid does not match current appgroup")
	}
	expected := helper.BuildSign(data, credential.AppSecret)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return nil, false, fmt.Errorf("signature verification failed")
	}

	for _, field := range signatureFields {
		delete(data, field)
	}
	return data, true, nil
}
