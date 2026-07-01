package application

import (
	"time"

	"github.com/gin-gonic/gin"
	cache "github.com/patrickmn/go-cache"
	"github.com/w7panel/w7panel-appid-proxy/app/application/http/controller"
	"github.com/w7panel/w7panel-appid-proxy/app/application/logic"
	"github.com/w7panel/w7panel-appid-proxy/common/service/k8s"
	"github.com/we7coreteam/w7-rangine-go/v2/pkg/support/facade"
	http_server "github.com/we7coreteam/w7-rangine-go/v2/src/http/server"
)

type Provider struct {
}

func (provider *Provider) Register(httpServer *http_server.Server) {
	provider.RegisterHttpRoutes(httpServer)
}

func (provider *Provider) RegisterHttpRoutes(httpServer *http_server.Server) {
	config := facade.GetConfig()

	k8sService, err := k8s.NewK8sService(config.GetString("kubernetes.config"))
	if err != nil {
		panic(err)
	}

	cacheTTL := time.Duration(config.GetInt("panel.credential_cache_seconds")) * time.Second
	if cacheTTL <= 0 {
		cacheTTL = 10 * time.Minute
	}
	negativeCacheTTL := time.Duration(config.GetInt("panel.credential_negative_cache_seconds")) * time.Second
	if negativeCacheTTL <= 0 {
		negativeCacheTTL = time.Minute
	}
	credentialLogic := &logic.Credential{
		Namespace:        config.GetString("panel.namespace"),
		Cache:            cache.New(cacheTTL, cacheTTL*2),
		NegativeCacheTTL: negativeCacheTTL,
		K8sService:       k8sService,
	}

	proxyController := controller.NewProxy(
		credentialLogic,
		config.GetString("proxy.scheme"),
		config.GetString("proxy.allowed_host"),
	)

	httpServer.RegisterRouters(func(engine *gin.Engine) {
		api := engine.Group("/api")
		api.GET("/live", proxyController.Live)
		api.GET("/app/info", proxyController.Credential)

		engine.NoRoute(proxyController.Proxy)
	})
}
