package controller

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/w7panel/w7panel-cloudnoauth/app/application/logic"
	"github.com/w7panel/w7panel-cloudnoauth/common/helper"
	"github.com/w7panel/w7panel-cloudnoauth/common/service/k8s"
	"github.com/we7coreteam/w7-rangine-go/v2/src/http/controller"
)

type Sidecar struct {
	controller.Abstract
	CredentialLogic *logic.Credential
}

func NewSidecar(credentialLogic *logic.Credential) Sidecar {
	return Sidecar{CredentialLogic: credentialLogic}
}

func (c Sidecar) Live(ctx *gin.Context) {
	c.JsonResponseWithoutError(ctx, gin.H{
		"message": "ok",
	})
}

func (c Sidecar) Credential(ctx *gin.Context) {
	slog.Info("credential resolve requested",
		"path", ctx.Request.URL.Path,
	)
	credential, err := c.CredentialLogic.Resolve(ctx.Request.Context())
	if err != nil {
		slog.Warn("credential resolve failed",
			"error", err,
		)
		c.JsonResponseWithServerError(ctx, err)
		return
	}
	slog.Info("credential resolve succeeded",
		"appgroup", credential.AppGroup,
		"appid", credential.AppID,
	)

	c.JsonResponseWithoutError(ctx, gin.H{
		"appid":     credential.AppID,
		"appsecret": credential.AppSecret,
	})
}

func (c Sidecar) VerifySignature(ctx *gin.Context) {
	data, err := parseSignedRequest(ctx.Request)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"valid": false})
		return
	}
	credential, err := c.CredentialLogic.Resolve(ctx.Request.Context())
	if err != nil {
		slog.Warn("signature credential resolve failed", "error", err)
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"valid": false})
		return
	}
	if err := verifySignature(data, credential); err != nil {
		slog.Warn("signature verification failed", "error", err)
		ctx.JSON(http.StatusUnauthorized, gin.H{"valid": false})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{
		"valid": true,
		"appid": credential.AppID,
	})
}

func parseSignedRequest(req *http.Request) (map[string]any, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if strings.Contains(req.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return helper.ParsePHPFormBody(body)
	}

	data := map[string]any{}
	if len(bytes.TrimSpace(body)) == 0 {
		return data, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return nil, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("request body must contain one JSON object")
	}
	return data, nil
}

func verifySignature(data map[string]any, credential k8s.AppCredential) error {
	signature, ok := data["sign"].(string)
	if !ok || strings.TrimSpace(signature) == "" {
		return fmt.Errorf("signature is required")
	}
	if fmt.Sprint(data["appid"]) != credential.AppID {
		return fmt.Errorf("appid does not match current appgroup")
	}
	expected := helper.BuildSign(data, credential.AppSecret)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}
