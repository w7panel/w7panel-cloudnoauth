package main

import (
	"bytes"
	_ "embed"

	"github.com/spf13/viper"
	"github.com/w7panel/w7panel-appid-proxy/app/application"
	app "github.com/we7coreteam/w7-rangine-go/v2/src"
	"github.com/we7coreteam/w7-rangine-go/v2/src/core/helper"
	ranginehttp "github.com/we7coreteam/w7-rangine-go/v2/src/http"
	ranginemiddleware "github.com/we7coreteam/w7-rangine-go/v2/src/http/middleware"
)

//go:embed config.yaml
var ConfigFileContent []byte

func main() {
	app := app.NewApp(app.Option{
		Name: "w7panel-appid-proxy",
		DefaultConfigLoader: func(config *viper.Viper) {
			config.SetConfigType("yaml")
			err := config.MergeConfig(bytes.NewReader(helper.ParseConfigContentEnv(ConfigFileContent)))
			if err != nil {
				panic(err)
			}
		},
	})
	// 业务中需要使用 http server，这里需要先实例化111
	httpServer := new(ranginehttp.Provider).Register(app.GetConfig(), app.GetConsole(), app.GetServerManager()).Export()
	// 注册一些全局中间件，路由或是其它一些全局操作
	httpServer.Use(ranginemiddleware.GetPanicHandlerMiddleware())

	// 注册业务 provider，此模块中需要使用 http server 和 console
	new(application.Provider).Register(httpServer)

	app.RunConsole()
}
