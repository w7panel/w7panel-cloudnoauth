package application

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/fcgi"
	"time"

	"github.com/gin-gonic/gin"
	cache "github.com/patrickmn/go-cache"
	"github.com/w7panel/w7panel-cloudnoauth/app/application/http/controller"
	"github.com/w7panel/w7panel-cloudnoauth/app/application/logic"
	"github.com/w7panel/w7panel-cloudnoauth/common/helper/net/listener"
	"github.com/w7panel/w7panel-cloudnoauth/common/service/k8s"
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
		PodName:          config.GetString("panel.pod_name"),
		Cache:            cache.New(cacheTTL, cacheTTL*2),
		NegativeCacheTTL: negativeCacheTTL,
		K8sService:       k8sService,
	}

	inbound, err := controller.NewInbound(
		credentialLogic,
		config.GetString("inbound.target_scheme"),
		fmt.Sprintf("%s:%d", config.GetString("inbound.target_host"), config.GetInt("inbound.target_port")),
	)
	if err != nil {
		panic(err)
	}
	inboundAddress := fmt.Sprintf("0.0.0.0:%d", config.GetInt("inbound.listen_port"))
	inboundListener, err := net.Listen("tcp", inboundAddress)
	if err != nil {
		panic(err)
	}
	httpListener, fastCGIListener := listener.SplitHTTPAndFastCGI(inboundListener)
	server := &http.Server{
		Addr:              inboundAddress,
		Handler:           inbound,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("inbound HTTP proxy listening", "address", server.Addr)
		if err := server.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			panic(err)
		}
	}()
	go func() {
		slog.Info("inbound FastCGI proxy listening", "address", inboundAddress)
		if err := fcgi.Serve(fastCGIListener, inbound); err != nil && !errors.Is(err, net.ErrClosed) {
			panic(err)
		}
	}()

	outbound, err := controller.NewOutboundWithUpstream(
		credentialLogic,
		config.GetString("outbound.scheme"),
		config.GetString("outbound.allowed_host"),
		config.GetString("outbound.upstream_host"),
		config.GetString("outbound.upstream_ca_file"),
	)
	if err != nil {
		panic(err)
	}
	sidecar := controller.NewSidecar(credentialLogic)
	httpServer.RegisterRouters(func(engine *gin.Engine) {
		api := engine.Group("/api")
		api.GET("/live", sidecar.Live)
		api.GET("/app/info", sidecar.Credential)

		engine.NoRoute(outbound.Forward)
	})
}
