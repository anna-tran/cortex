package runtimeconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"github.com/cortexproject/cortex/pkg/storage/bucket"
	"github.com/thanos-io/objstore"
	"io"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/cortexproject/cortex/pkg/util/services"
)

type BucketClientFactory func(ctx context.Context) (objstore.Bucket, error)

// Loader loads the configuration from file.
type Loader func(r io.Reader) (interface{}, error)

// Config holds the config for an Manager instance.
// It holds config related to loading per-tenant config.
type Config struct {
	ReloadPeriod time.Duration `yaml:"period"`
	// LoadPath contains the path to the runtime config file, requires an
	// non-empty value
	LoadPath string `yaml:"file"`
	Loader   Loader `yaml:"-"`

	StorageConfig bucket.Config `yaml:",inline"`
}

// RegisterFlags registers flags.
func (mc *Config) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&mc.LoadPath, "runtime-config.file", "", "File with the configuration that can be updated in runtime.")
	f.DurationVar(&mc.ReloadPeriod, "runtime-config.reload-period", 10*time.Second, "How often to check runtime config file.")

	mc.StorageConfig.RegisterFlagsWithPrefixAndBackend("runtime-config.", f, bucket.Filesystem)
}

// Manager periodically reloads the configuration from a file, and keeps this
// configuration available for clients.
type Manager struct {
	services.Service

	cfg    Config
	logger log.Logger

	listenersMtx sync.Mutex
	listeners    []chan interface{}

	configMtx sync.RWMutex
	config    interface{}

	configLoadSuccess prometheus.Gauge
	configHash        *prometheus.GaugeVec

	bucketClient        objstore.Bucket
	bucketClientFactory BucketClientFactory
}

// New creates an instance of Manager and starts reload config loop based on config
func New(cfg Config, registerer prometheus.Registerer, logger log.Logger, factory BucketClientFactory) (*Manager, error) {
	if cfg.LoadPath == "" {
		return nil, errors.New("LoadPath is empty")
	}

	if cfg.StorageConfig.Backend == "" {
		cfg.StorageConfig.Backend = bucket.Filesystem
	}

	mgr := Manager{
		cfg: cfg,
		configLoadSuccess: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Name: "runtime_config_last_reload_successful",
			Help: "Whether the last runtime-config reload attempt was successful.",
		}),
		configHash: promauto.With(registerer).NewGaugeVec(prometheus.GaugeOpts{
			Name: "runtime_config_hash",
			Help: "Hash of the currently active runtime config file.",
		}, []string{"sha256"}),
		logger:              logger,
		bucketClientFactory: factory,
	}

	mgr.Service = services.NewBasicService(mgr.starting, mgr.loop, mgr.stopping)
	return &mgr, nil
}

func (om *Manager) starting(ctx context.Context) error {
	if om.cfg.LoadPath == "" {
		return nil
	}

	var err error
	om.bucketClient, err = om.bucketClientFactory(ctx)
	if err != nil {
		return err
	}

	return errors.Wrap(om.loadConfig(ctx), "failed to load runtime config")
}

// CreateListenerChannel creates new channel that can be used to receive new config values.
// If there is no receiver waiting for value when config manager tries to send the update,
// or channel buffer is full, update is discarded.
//
// When config manager is stopped, it closes all channels to notify receivers that they will
// not receive any more updates.
func (om *Manager) CreateListenerChannel(buffer int) <-chan interface{} {
	ch := make(chan interface{}, buffer)

	om.listenersMtx.Lock()
	defer om.listenersMtx.Unlock()

	om.listeners = append(om.listeners, ch)
	return ch
}

// CloseListenerChannel removes given channel from list of channels to send notifications to and closes channel.
func (om *Manager) CloseListenerChannel(listener <-chan interface{}) {
	om.listenersMtx.Lock()
	defer om.listenersMtx.Unlock()

	for ix, ch := range om.listeners {
		if ch == listener {
			om.listeners = append(om.listeners[:ix], om.listeners[ix+1:]...)
			close(ch)
			break
		}
	}
}

func (om *Manager) loop(ctx context.Context) error {
	if om.cfg.LoadPath == "" {
		level.Info(om.logger).Log("msg", "runtime config disabled: file not specified")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(om.cfg.ReloadPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := om.loadConfig(ctx)
			if err != nil {
				// Log but don't stop on error - we don't want to halt all ingesters because of a typo
				level.Error(om.logger).Log("msg", "failed to load config", "err", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// loadConfig loads configuration using the loader function, and if successful,
// stores it as current configuration and notifies listeners.
func (om *Manager) loadConfig(ctx context.Context) error {
	buf, err := om.loadConfigFromBucket(ctx)

	if err != nil {
		om.configLoadSuccess.Set(0)
		return errors.Wrap(err, "read file")
	}
	hash := sha256.Sum256(buf)

	cfg, err := om.cfg.Loader(bytes.NewReader(buf))
	if err != nil {
		om.configLoadSuccess.Set(0)
		return errors.Wrap(err, "load file")
	}
	om.configLoadSuccess.Set(1)

	om.setConfig(cfg)
	om.callListeners(cfg)

	// expose hash of runtime config
	om.configHash.Reset()
	om.configHash.WithLabelValues(fmt.Sprintf("%x", hash[:])).Set(1)
	return nil
}

func (om *Manager) loadConfigFromBucket(ctx context.Context) ([]byte, error) {
	readCloser, err := om.bucketClient.Get(ctx, om.cfg.LoadPath)
	if err != nil {
		return nil, errors.Wrap(err, "open file")
	}

	buf, err := io.ReadAll(readCloser)
	if err != nil {
		return nil, errors.Wrap(err, "read entire file")
	}

	err = readCloser.Close()
	return buf, err
}

func (om *Manager) setConfig(config interface{}) {
	om.configMtx.Lock()
	defer om.configMtx.Unlock()
	om.config = config
}

func (om *Manager) callListeners(newValue interface{}) {
	om.listenersMtx.Lock()
	defer om.listenersMtx.Unlock()

	for _, ch := range om.listeners {
		select {
		case ch <- newValue:
			// ok
		default:
			// nobody is listening or buffer full.
		}
	}
}

// Stop stops the Manager
func (om *Manager) stopping(_ error) error {
	om.listenersMtx.Lock()
	defer om.listenersMtx.Unlock()

	for _, ch := range om.listeners {
		close(ch)
	}
	om.listeners = nil
	return nil
}

// GetConfig returns last loaded config value, possibly nil.
func (om *Manager) GetConfig() interface{} {
	om.configMtx.RLock()
	defer om.configMtx.RUnlock()

	return om.config
}
