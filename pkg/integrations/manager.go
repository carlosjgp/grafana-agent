package integrations

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"sync"
	"time"

	config_util "github.com/prometheus/common/config"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gorilla/mux"
	"github.com/grafana/agent/pkg/prom"
	"github.com/grafana/agent/pkg/prom/instance"
	"github.com/grafana/agent/pkg/prom/instance/configstore"
	"github.com/grafana/agent/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/pkg/relabel"
)

var (
	integrationAbnormalExits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_prometheus_integration_abnormal_exits_total",
		Help: "Total number of times an agent integration exited unexpectedly, causing it to be restarted.",
	}, []string{"integration_name"})
)

// DefaultManagerConfig holds the default settings for integrations.
var DefaultManagerConfig = ManagerConfig{
	ScrapeIntegrations:        true,
	IntegrationRestartBackoff: 5 * time.Second,
	UseHostnameLabel:          true,
	ReplaceInstanceLabel:      true,
}

// ManagerConfig holds the configuration for all integrations.
type ManagerConfig struct {
	// When true, scrapes metrics from integrations.
	ScrapeIntegrations bool `yaml:"scrape_integrations,omitempty"`
	// When true, replaces the instance label with the agent hostname.
	ReplaceInstanceLabel bool `yaml:"replace_instance_label,omitempty"`

	// DEPRECATED. When true, adds an agent_hostname label to all samples from integrations.
	// ReplaceInstanceLabel should be used instead.
	UseHostnameLabel bool `yaml:"use_hostname_label,omitempty"`

	// The integration configs is merged with the manager config struct so we
	// don't want to export it here; we'll manually unmarshal it in UnmarshalYAML.
	Integrations Configs `yaml:"-"`

	// Extra labels to add for all integration samples
	Labels model.LabelSet `yaml:"labels,omitempty"`

	// Prometheus RW configs to use for all integrations.
	PrometheusRemoteWrite []*instance.RemoteWriteConfig `yaml:"prometheus_remote_write,omitempty"`

	IntegrationRestartBackoff time.Duration `yaml:"integration_restart_backoff,omitempty"`

	// ListenPort tells the integration Manager which port the Agent is
	// listening on for generating Prometheus instance configs.
	ListenPort int `yaml:"-"`

	// ListenHost tells the integration Manager which port the Agent is
	// listening on for generating Prometheus instance configs
	ListenHost string `yaml:"-"`

	TLSConfig config_util.TLSConfig `yaml:"http_tls_config,omitempty"`

	// This is set to true if the Server TLSConfig Cert and Key path are set
	ServerUsingTLS bool `yaml:"-"`
}

// MarshalYAML implements yaml.Marshaler for ManagerConfig.
func (c ManagerConfig) MarshalYAML() (interface{}, error) {
	return MarshalYAML(c)
}

// UnmarshalYAML implements yaml.Unmarshaler for ManagerConfig.
func (c *ManagerConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultManagerConfig
	return UnmarshalYAML(c, unmarshal)
}

// DefaultRelabelConfigs returns the set of relabel configs that should be
// prepended to all RelabelConfigs for an integration.
func (c *ManagerConfig) DefaultRelabelConfigs(hostname string) []*relabel.Config {
	var cfgs []*relabel.Config

	if c.ReplaceInstanceLabel {
		replacement := fmt.Sprintf("%s:%d", hostname, c.ListenPort)

		cfgs = append(cfgs, &relabel.Config{
			SourceLabels: model.LabelNames{model.AddressLabel},
			Action:       relabel.Replace,
			Separator:    ";",
			Regex:        relabel.MustNewRegexp("(.*)"),
			Replacement:  replacement,
			TargetLabel:  model.InstanceLabel,
		})
	}

	return cfgs
}

// ApplyDefaults applies default settings to the ManagerConfig and validates
// that it can be used.
//
// If any integrations are enabled and are configured to be scraped, the
// Prometheus configuration must have a WAL directory configured.
func (c *ManagerConfig) ApplyDefaults(cfg *prom.Config) error {
	for _, ic := range c.Integrations {
		if !ic.CommonConfig().Enabled {
			continue
		}

		scrapeIntegration := c.ScrapeIntegrations
		if common := ic.CommonConfig(); common.ScrapeIntegration != nil {
			scrapeIntegration = *common.ScrapeIntegration
		}

		// WAL must be configured if an integration is going to be scraped.
		if scrapeIntegration && cfg.WALDir == "" {
			return fmt.Errorf("no wal_directory configured")
		}
	}

	return nil
}

// Manager manages a set of integrations and runs them.
type Manager struct {
	logger log.Logger

	cfgMut sync.RWMutex
	cfg    ManagerConfig

	hostname string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	im        instance.Manager
	validator configstore.Validator

	integrationsMut sync.RWMutex
	integrations    map[string]*integrationProcess
}

// NewManager creates a new integrations manager. NewManager must be given an
// InstanceManager which is responsible for accepting instance configs to
// scrape and send metrics from running integrations.
func NewManager(c ManagerConfig, logger log.Logger, im instance.Manager, validate configstore.Validator) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())

	m := &Manager{
		logger: logger,

		ctx:    ctx,
		cancel: cancel,

		im:        im,
		validator: validate,

		integrations: make(map[string]*integrationProcess, len(c.Integrations)),
	}

	var err error
	m.hostname, err = instance.Hostname()
	if err != nil {
		return nil, err
	}

	if err := m.ApplyConfig(c); err != nil {
		return nil, fmt.Errorf("failed applying config: %w", err)
	}
	return m, nil
}

// ApplyConfig updates the configuration of the integrations subsystem.
func (m *Manager) ApplyConfig(cfg ManagerConfig) error {
	var failed bool

	m.cfgMut.Lock()
	defer m.cfgMut.Unlock()

	m.integrationsMut.Lock()
	defer m.integrationsMut.Unlock()

	if util.CompareYAML(m.cfg, cfg) {
		return nil
	}

	select {
	case <-m.ctx.Done():
		return fmt.Errorf("Manager already stopped")
	default:
		// No-op
	}

	// Iterate over our integrations. New or changed integrations will be
	// started, with their existing counterparts being shut down.
	for _, ic := range cfg.Integrations {
		// Key is used to identify the instance of this integration within the
		// instance manager and within our set of running integrations.
		key := integrationKey(ic.Name())

		// Look for an existing integration with the same key. If it exists and
		// is unchanged, we have nothing to do. Otherwise, we're going to recreate
		// it with the new settings, so we'll need to stop it.
		if p, exist := m.integrations[key]; exist {
			if util.CompareYAML(p.cfg, ic) {
				continue
			}
			p.stop()
			delete(m.integrations, key)
		}

		l := log.With(m.logger, "integration", ic.Name())
		i, err := ic.NewIntegration(l)
		if err != nil {
			level.Error(m.logger).Log("msg", "failed to initialize integration. it will not run or be scraped", "integration", ic.Name(), "err", err)
			failed = true

			// If this integration was running before, its instance won't be cleaned
			// up since it's now removed from the map. We need to clean it up here.
			_ = m.im.DeleteConfig(key)
			continue
		}

		// Create, start, and register the new integration.
		ctx, cancel := context.WithCancel(m.ctx)
		p := &integrationProcess{
			log: m.logger,
			cfg: ic,
			i:   i,

			ctx:  ctx,
			stop: cancel,

			wg:   &m.wg,
			wait: m.instanceBackoff,
		}
		go p.Run()
		m.integrations[key] = p
	}

	// Delete instances and processed that have been removed in between calls to
	// ApplyConfig.
	for key, process := range m.integrations {
		foundConfig := false
		for _, ic := range cfg.Integrations {
			if integrationKey(ic.Name()) == key {
				foundConfig = true
				break
			}
		}
		if foundConfig {
			continue
		}

		_ = m.im.DeleteConfig(key)
		process.stop()
		delete(m.integrations, key)
	}

	// Re-apply configs to our instance manager for all running integrations.
	// Generated scrape configs may change in between calls to ApplyConfig even
	// if the configs for the integration didn't.
	for key, p := range m.integrations {
		shouldCollect := cfg.ScrapeIntegrations
		if common := p.cfg.CommonConfig(); common.ScrapeIntegration != nil {
			shouldCollect = *common.ScrapeIntegration
		}

		switch shouldCollect {
		case true:
			instanceConfig := m.instanceConfigForIntegration(p.cfg, p.i, cfg)
			if err := m.validator(&instanceConfig); err != nil {
				level.Error(p.log).Log("msg", "failed to validate generated scrape config for integration. integration will not be scraped", "err", err, "integration", p.cfg.Name())
				failed = true
				break
			}

			if err := m.im.ApplyConfig(instanceConfig); err != nil {
				level.Error(p.log).Log("msg", "failed to apply integration. integration will not be scraped", "err", err, "integration", p.cfg.Name())
				failed = true
			}
		case false:
			// If a previous instance of the config was being scraped, we need to
			// delete it here. Calling DeleteConfig when nothing is running is a safe
			// operation.
			_ = m.im.DeleteConfig(key)
		}
	}

	m.cfg = cfg

	if failed {
		return fmt.Errorf("not all integrations were correctly updated")
	}
	return nil
}

// integrationProcess is a running integration.
type integrationProcess struct {
	log  log.Logger
	ctx  context.Context
	stop context.CancelFunc
	cfg  Config
	i    Integration

	wg   *sync.WaitGroup
	wait func(cfg Config, err error)
}

// Run runs the integration until the process is canceled.
func (p *integrationProcess) Run() {
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("%v", r)
			level.Error(p.log).Log("msg", "integration has panicked. THIS IS A BUG!", "err", err, "integration", p.cfg.Name())
		}
	}()

	p.wg.Add(1)
	defer p.wg.Done()

	for {
		err := p.i.Run(p.ctx)
		if err != nil && err != context.Canceled {
			p.wait(p.cfg, err)
		} else {
			level.Info(p.log).Log("msg", "stopped integration", "integration", p.cfg.Name())
			break
		}
	}
}

func (m *Manager) instanceBackoff(cfg Config, err error) {
	m.cfgMut.RLock()
	defer m.cfgMut.RUnlock()

	integrationAbnormalExits.WithLabelValues(cfg.Name()).Inc()
	level.Error(m.logger).Log("msg", "integration stopped abnormally, restarting after backoff", "err", err, "integration", cfg.Name(), "backoff", m.cfg.IntegrationRestartBackoff)
	time.Sleep(m.cfg.IntegrationRestartBackoff)
}

func (m *Manager) instanceConfigForIntegration(icfg Config, i Integration, cfg ManagerConfig) instance.Config {
	common := icfg.CommonConfig()
	relabelConfigs := append(cfg.DefaultRelabelConfigs(m.hostname), common.RelabelConfigs...)

	schema := "http"
	// Check for HTTPS support
	var httpClientConfig config_util.HTTPClientConfig
	if cfg.ServerUsingTLS {
		schema = "https"
		httpClientConfig.TLSConfig = cfg.TLSConfig
	}

	var scrapeConfigs []*config.ScrapeConfig

	for _, isc := range i.ScrapeConfigs() {
		sc := &config.ScrapeConfig{
			JobName:                 fmt.Sprintf("integrations/%s", isc.JobName),
			MetricsPath:             path.Join("/integrations", icfg.Name(), isc.MetricsPath),
			Scheme:                  schema,
			HonorLabels:             false,
			HonorTimestamps:         true,
			ScrapeInterval:          model.Duration(common.ScrapeInterval),
			ScrapeTimeout:           model.Duration(common.ScrapeTimeout),
			ServiceDiscoveryConfigs: m.scrapeServiceDiscovery(cfg),
			RelabelConfigs:          relabelConfigs,
			MetricRelabelConfigs:    common.MetricRelabelConfigs,
			HTTPClientConfig:        httpClientConfig,
		}

		scrapeConfigs = append(scrapeConfigs, sc)
	}

	instanceCfg := instance.DefaultConfig
	instanceCfg.Name = integrationKey(icfg.Name())
	instanceCfg.ScrapeConfigs = scrapeConfigs
	instanceCfg.RemoteWrite = cfg.PrometheusRemoteWrite
	if common.WALTruncateFrequency > 0 {
		instanceCfg.WALTruncateFrequency = common.WALTruncateFrequency
	}
	return instanceCfg
}

// integrationKey returns the key for an integration Config, used for its
// instance name and name in the process cache.
func integrationKey(name string) string {
	return fmt.Sprintf("integration/%s", name)
}

func (m *Manager) scrapeServiceDiscovery(cfg ManagerConfig) discovery.Configs {
	// A blank host somehow works, but it then requires a sever name to be set under tls.
	newHost := cfg.ListenHost
	if newHost == "" {
		newHost = "127.0.0.1"
	}
	localAddr := fmt.Sprintf("%s:%d", newHost, cfg.ListenPort)
	labels := model.LabelSet{}
	if cfg.UseHostnameLabel {
		labels[model.LabelName("agent_hostname")] = model.LabelValue(m.hostname)
	}
	for k, v := range cfg.Labels {
		labels[k] = v
	}

	return discovery.Configs{
		discovery.StaticConfig{{
			Targets: []model.LabelSet{{model.AddressLabel: model.LabelValue(localAddr)}},
			Labels:  labels,
		}},
	}
}

// WireAPI hooks up /metrics routes per-integration.
func (m *Manager) WireAPI(r *mux.Router) {
	type handlerCacheEntry struct {
		handler http.Handler
		process *integrationProcess
	}
	var (
		handlerMut   sync.Mutex
		handlerCache = make(map[string]handlerCacheEntry)
	)

	// loadHandler will perform a dynamic lookup of an HTTP handler for an
	// integration. loadHandler should be called with a read lock on the
	// integrations mutex.
	loadHandler := func(key string) http.Handler {
		handlerMut.Lock()
		defer handlerMut.Unlock()

		// Search the integration by name to see if it's still running.
		p, ok := m.integrations[key]
		if !ok {
			delete(handlerCache, key)
			return http.NotFoundHandler()
		}

		// Now look in the cache for a handler for the running process.
		cacheEntry, ok := handlerCache[key]
		if ok && cacheEntry.process == p {
			return cacheEntry.handler
		}

		// New integration process that hasn't been scraped before. Generate
		// a handler for it and cache it.
		handler, err := p.i.MetricsHandler()
		if err != nil {
			level.Error(m.logger).Log("msg", "could not create http handler for integration", "integration", p.cfg.Name(), "err", err)
			return http.HandlerFunc(internalServiceError)
		}

		cacheEntry = handlerCacheEntry{handler: handler, process: p}
		handlerCache[key] = cacheEntry
		return cacheEntry.handler
	}

	r.HandleFunc("/integrations/{name}/metrics", func(rw http.ResponseWriter, r *http.Request) {
		m.integrationsMut.RLock()
		defer m.integrationsMut.RUnlock()

		key := integrationKey(mux.Vars(r)["name"])
		handler := loadHandler(key)
		handler.ServeHTTP(rw, r)
	})
}

func internalServiceError(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
}

// Stop stops the manager and all of its integrations. Blocks until all running
// integrations exit.
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
}
