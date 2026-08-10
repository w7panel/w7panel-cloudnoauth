package controller

import (
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/w7panel/w7panel-cloudnoauth/app/application/logic"
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
