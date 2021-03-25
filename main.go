package main

import (
	"github.com/nspcc-dev/cdn-sdk/logger"
	"github.com/nspcc-dev/neofs-http-gate/global"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

func newLogger(v *viper.Viper) *zap.Logger {
	options := []logger.Option{
		logger.WithLevel(v.GetString(cfgLoggerLevel)),
		logger.WithTraceLevel(v.GetString(cfgLoggerTraceLevel)),

		logger.WithFormat(v.GetString(cfgLoggerFormat)),

		logger.WithSamplingInitial(v.GetInt(cfgLoggerSamplingInitial)),
		logger.WithSamplingThereafter(v.GetInt(cfgLoggerSamplingThereafter)),

		logger.WithAppName(v.GetString(cfgApplicationName)),
		logger.WithAppVersion(v.GetString(cfgApplicationVersion)),
	}

	if v.GetBool(cfgLoggerNoCaller) {
		options = append(options, logger.WithoutCaller())
	}

	if v.GetBool(cfgLoggerNoDisclaimer) {
		options = append(options, logger.WithoutDisclaimer())
	}

	l, err := logger.New(options...)
	if err != nil {
		panic(err)
	}

	return l
}

func main() {
	var (
		v = settings()
		l = newLogger(v)
		g = global.Context()
		a = newApp(g, WithLogger(l), WithConfig(v))
	)
	go a.Serve(g)
	go a.Worker(g)
	a.Wait()
}
