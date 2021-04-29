package main

import (
	"context"
	"math"
	"strconv"

	"github.com/fasthttp/router"
	"github.com/nspcc-dev/neofs-http-gate/connections"
	"github.com/nspcc-dev/neofs-http-gate/downloader"
	"github.com/nspcc-dev/neofs-http-gate/logger"
	"github.com/nspcc-dev/neofs-http-gate/neofs"
	"github.com/nspcc-dev/neofs-http-gate/uploader"
	"github.com/spf13/viper"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
	"google.golang.org/grpc/grpclog"
)

type (
	app struct {
		log          *zap.Logger
		plant        neofs.ClientPlant
		cfg          *viper.Viper
		auxiliaryLog logger.Logger
		webServer    *fasthttp.Server
		webDone      chan struct{}
	}

	App interface {
		Wait()
		Serve(context.Context)
	}

	Option func(a *app)
)

func WithLogger(l *zap.Logger) Option {
	return func(a *app) {
		if l == nil {
			return
		}
		a.log = l
	}
}

func WithConfig(c *viper.Viper) Option {
	return func(a *app) {
		if c == nil {
			return
		}
		a.cfg = c
	}
}

func newApp(ctx context.Context, opt ...Option) App {
	var (
		creds neofs.Credentials
		err   error
	)

	a := &app{
		log:       zap.L(),
		cfg:       viper.GetViper(),
		webServer: new(fasthttp.Server),
		webDone:   make(chan struct{}),
	}
	for i := range opt {
		opt[i](a)
	}
	a.auxiliaryLog = logger.GRPC(a.log)
	if a.cfg.GetBool(cmdVerbose) {
		grpclog.SetLoggerV2(a.auxiliaryLog)
	}
	// -- setup FastHTTP server --
	a.webServer.Name = "neofs-http-gate"
	a.webServer.ReadBufferSize = a.cfg.GetInt(cfgWebReadBufferSize)
	a.webServer.WriteBufferSize = a.cfg.GetInt(cfgWebWriteBufferSize)
	a.webServer.ReadTimeout = a.cfg.GetDuration(cfgWebReadTimeout)
	a.webServer.WriteTimeout = a.cfg.GetDuration(cfgWebWriteTimeout)
	a.webServer.DisableHeaderNamesNormalizing = true
	a.webServer.NoDefaultServerHeader = true
	a.webServer.NoDefaultContentType = true
	a.webServer.MaxRequestBodySize = a.cfg.GetInt(cfgWebMaxRequestBodySize)
	a.webServer.DisablePreParseMultipartForm = true
	a.webServer.StreamRequestBody = a.cfg.GetBool(cfgWebStreamRequestBody)
	// -- -- -- -- -- -- -- -- -- -- -- -- -- --
	keystring := a.cfg.GetString(cmdNeoFSKey)
	if len(keystring) == 0 {
		a.log.Info("no key specified, creating one automatically for this run")
		creds, err = neofs.NewEphemeralCredentials()
	} else {
		creds, err = neofs.NewCredentials(keystring)
	}
	if err != nil {
		a.log.Fatal("failed to get neofs credentials", zap.Error(err))
	}
	pb := new(connections.PoolBuilder)
	for i := 0; ; i++ {
		address := a.cfg.GetString(cfgPeers + "." + strconv.Itoa(i) + ".address")
		weight := a.cfg.GetFloat64(cfgPeers + "." + strconv.Itoa(i) + ".weight")
		if address == "" {
			break
		}
		if weight <= 0 { // unspecified or wrong
			weight = 1
		}
		pb.AddNode(address, weight)
		a.log.Info("add connection", zap.String("address", address), zap.Float64("weight", weight))
	}
	opts := &connections.PoolBuilderOptions{
		Key:                     creds.PrivateKey(),
		NodeConnectionTimeout:   a.cfg.GetDuration(cfgConTimeout),
		NodeRequestTimeout:      a.cfg.GetDuration(cfgReqTimeout),
		ClientRebalanceInterval: a.cfg.GetDuration(cfgRebalance),
		SessionExpirationEpoch:  math.MaxUint64,
		KeepaliveTime:           a.cfg.GetDuration(cfgKeepaliveTime),
		KeepaliveTimeout:        a.cfg.GetDuration(cfgKeepaliveTimeout),
		KeepalivePermitWoStream: a.cfg.GetBool(cfgKeepalivePermitWithoutStream),
	}
	pool, err := pb.Build(ctx, opts)
	if err != nil {
		a.log.Fatal("failed to create connection pool", zap.Error(err))
	}
	a.plant, err = neofs.NewClientPlant(ctx, pool, creds)
	if err != nil {
		a.log.Fatal("failed to create neofs client plant")
	}
	return a
}

func (a *app) Wait() {
	a.log.Info("starting application")
	<-a.webDone // wait for web-server to be stopped
}

func (a *app) Serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		a.log.Info("shutting down web server", zap.Error(a.webServer.Shutdown()))
		close(a.webDone)
	}()
	edts := a.cfg.GetBool(cfgUploaderHeaderEnableDefaultTimestamp)
	uploader := uploader.New(a.log, a.plant, edts)
	downloader, err := downloader.New(ctx, a.log, a.plant)
	if err != nil {
		a.log.Fatal("failed to create downloader", zap.Error(err))
	}
	// Configure router.
	r := router.New()
	r.RedirectTrailingSlash = true
	r.POST("/upload/{cid}", uploader.Upload)
	a.log.Info("added path /upload/{cid}")
	r.GET("/get/{cid}/{oid}", downloader.DownloadByAddress)
	a.log.Info("added path /get/{cid}/{oid}")
	r.GET("/get_by_attribute/{cid}/{attr_key}/{attr_val:*}", downloader.DownloadByAttribute)
	a.log.Info("added path /get_by_attribute/{cid}/{attr_key}/{attr_val:*}")
	// enable metrics
	if a.cfg.GetBool(cmdMetrics) {
		a.log.Info("added path /metrics/")
		attachMetrics(r, a.auxiliaryLog)
	}
	// enable pprof
	if a.cfg.GetBool(cmdPprof) {
		a.log.Info("added path /debug/pprof/")
		attachProfiler(r)
	}
	bind := a.cfg.GetString(cfgListenAddress)
	tlsCertPath := a.cfg.GetString(cfgTLSCertificate)
	tlsKeyPath := a.cfg.GetString(cfgTLSKey)

	a.webServer.Handler = r.Handler
	if tlsCertPath == "" && tlsKeyPath == "" {
		a.log.Info("running web server", zap.String("address", bind))
		err = a.webServer.ListenAndServe(bind)
	} else {
		a.log.Info("running web server (TLS-enabled)", zap.String("address", bind))
		err = a.webServer.ListenAndServeTLS(bind, tlsCertPath, tlsKeyPath)
	}
	if err != nil {
		a.log.Fatal("could not start server", zap.Error(err))
	}
}
