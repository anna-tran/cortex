package querier

import (
	"flag"
	"time"

	"github.com/go-kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cortexproject/cortex/pkg/ring/client"
	"github.com/cortexproject/cortex/pkg/storegateway/storegatewaypb"
	"github.com/cortexproject/cortex/pkg/util/grpcclient"
	"github.com/cortexproject/cortex/pkg/util/tls"
)

func newStoreGatewayClientFactory(clientCfg grpcclient.ConfigWithHealthCheck, reg prometheus.Registerer) client.PoolFactory {
	requestDuration := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "cortex",
		Name:        "storegateway_client_request_duration_seconds",
		Help:        "Time spent executing requests to the store-gateway.",
		Buckets:     prometheus.ExponentialBuckets(0.008, 4, 7),
		ConstLabels: prometheus.Labels{"client": "querier"},
	}, []string{"operation", "status_code"})

	return func(addr string) (client.PoolClient, error) {
		return dialStoreGatewayClient(clientCfg, addr, requestDuration)
	}
}

func dialStoreGatewayClient(clientCfg grpcclient.ConfigWithHealthCheck, addr string, requestDuration *prometheus.HistogramVec) (*storeGatewayClient, error) {
	opts, err := clientCfg.DialOption(grpcclient.Instrument(requestDuration))
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to dial store-gateway %s", addr)
	}

	return &storeGatewayClient{
		StoreGatewayClient: storegatewaypb.NewStoreGatewayClient(conn),
		HealthClient:       grpc_health_v1.NewHealthClient(conn),
		conn:               conn,
	}, nil
}

type storeGatewayClient struct {
	storegatewaypb.StoreGatewayClient
	grpc_health_v1.HealthClient
	conn *grpc.ClientConn
}

func (c *storeGatewayClient) Close() error {
	return c.conn.Close()
}

func (c *storeGatewayClient) String() string {
	return c.RemoteAddress()
}

func (c *storeGatewayClient) RemoteAddress() string {
	return c.conn.Target()
}

func newStoreGatewayClientPool(discovery client.PoolServiceDiscovery, clientConfig ClientConfig, logger log.Logger, reg prometheus.Registerer) *client.Pool {
	// We prefer sane defaults instead of exposing further config options.
	clientCfg := grpcclient.ConfigWithHealthCheck{
		Config: grpcclient.Config{
			MaxRecvMsgSize:      100 << 20,
			MaxSendMsgSize:      16 << 20,
			GRPCCompression:     clientConfig.GRPCCompression,
			RateLimit:           0,
			RateLimitBurst:      0,
			BackoffOnRatelimits: false,
			TLSEnabled:          clientConfig.TLSEnabled,
			TLS:                 clientConfig.TLS,
			ConnectTimeout:      clientConfig.ConnectTimeout,
		},
		HealthCheckConfig: clientConfig.HealthCheckConfig,
	}
	poolCfg := client.PoolConfig{
		CheckInterval:      time.Minute,
		HealthCheckEnabled: true,
		HealthCheckTimeout: 10 * time.Second,
	}

	clientsCount := promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Namespace:   "cortex",
		Name:        "storegateway_clients",
		Help:        "The current number of store-gateway clients in the pool.",
		ConstLabels: map[string]string{"client": "querier"},
	})

	return client.NewPool("store-gateway", poolCfg, discovery, newStoreGatewayClientFactory(clientCfg, reg), clientsCount, logger)
}

type ClientConfig struct {
	TLSEnabled        bool                         `yaml:"tls_enabled"`
	TLS               tls.ClientConfig             `yaml:",inline"`
	GRPCCompression   string                       `yaml:"grpc_compression"`
	HealthCheckConfig grpcclient.HealthCheckConfig `yaml:"healthcheck_config" doc:"description=EXPERIMENTAL: If enabled, gRPC clients perform health checks for each target and fail the request if the target is marked as unhealthy."`
	ConnectTimeout    time.Duration                `yaml:"connect_timeout"`
}

func (cfg *ClientConfig) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
	f.BoolVar(&cfg.TLSEnabled, prefix+".tls-enabled", cfg.TLSEnabled, "Enable TLS for gRPC client connecting to store-gateway.")
	f.StringVar(&cfg.GRPCCompression, prefix+".grpc-compression", "", "Use compression when sending messages. Supported values are: 'gzip', 'snappy' and '' (disable compression)")
	f.DurationVar(&cfg.ConnectTimeout, prefix+".connect-timeout", 5*time.Second, "The maximum amount of time to establish a connection. A value of 0 means using default gRPC client connect timeout 5s.")
	cfg.TLS.RegisterFlagsWithPrefix(prefix, f)
	cfg.HealthCheckConfig.RegisterFlagsWithPrefix(prefix, f)
}
