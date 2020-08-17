package autoconf

import (
	"context"
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	"github.com/hashicorp/consul/agent/cache"
	"github.com/hashicorp/consul/agent/config"
	"github.com/hashicorp/consul/agent/token"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/consul/logging"
	"github.com/hashicorp/consul/proto/pbautoconf"
	"github.com/hashicorp/go-hclog"
)

// AutoConfig is all the state necessary for being able to parse a configuration
// as well as perform the necessary RPCs to perform Agent Auto Configuration.
type AutoConfig struct {
	sync.Mutex

	acConfig           Config
	logger             hclog.Logger
	cache              Cache
	waiter             *lib.RetryWaiter
	config             *config.RuntimeConfig
	autoConfigResponse *pbautoconf.AutoConfigResponse
	autoConfigSource   config.Source

	running bool
	done    chan struct{}
	// cancel is used to cancel the entire AutoConfig
	// go routine. This is the main field protected
	// by the mutex as it being non-nil indicates that
	// the go routine has been started and is stoppable.
	// note that it doesn't indcate that the go routine
	// is currently running.
	cancel context.CancelFunc

	// cancelWatches is used to cancel the existing
	// cache watches regarding the agents certificate. This is
	// mainly only necessary when the Agent token changes.
	cancelWatches context.CancelFunc

	// cacheUpdates is the chan used to have the cache
	// send us back events
	cacheUpdates chan cache.UpdateEvent

	// tokenUpdates is the struct used to receive
	// events from the token store when the Agent
	// token is updated.
	tokenUpdates token.Notifier
}

// New creates a new AutoConfig object for providing automatic Consul configuration.
func New(config Config) (*AutoConfig, error) {
	switch {
	case config.Loader == nil:
		return nil, fmt.Errorf("must provide a config loader")
	case config.DirectRPC == nil:
		return nil, fmt.Errorf("must provide a direct RPC delegate")
	case config.Cache == nil:
		return nil, fmt.Errorf("must provide a cache")
	case config.TLSConfigurator == nil:
		return nil, fmt.Errorf("must provide a TLS configurator")
	case config.Tokens == nil:
		return nil, fmt.Errorf("must provide a token store")
	}

	if config.FallbackLeeway == 0 {
		config.FallbackLeeway = 10 * time.Second
	}
	if config.FallbackRetry == 0 {
		config.FallbackRetry = time.Minute
	}

	logger := config.Logger
	if logger == nil {
		logger = hclog.NewNullLogger()
	} else {
		logger = logger.Named(logging.AutoConfig)
	}

	if config.Waiter == nil {
		config.Waiter = lib.NewRetryWaiter(1, 0, 10*time.Minute, lib.NewJitterRandomStagger(25))
	}

	return &AutoConfig{
		acConfig: config,
		logger:   logger,
	}, nil
}

// ReadConfig will parse the current configuration and inject any
// auto-config sources if present into the correct place in the parsing chain.
func (ac *AutoConfig) ReadConfig() (*config.RuntimeConfig, error) {
	cfg, warnings, err := ac.acConfig.Loader(ac.autoConfigSource)
	if err != nil {
		return cfg, err
	}

	for _, w := range warnings {
		ac.logger.Warn(w)
	}

	ac.config = cfg
	return cfg, nil
}

// InitialConfiguration will perform a one-time RPC request to the configured servers
// to retrieve various cluster wide configurations. See the proto/pbautoconf/auto_config.proto
// file for a complete reference of what configurations can be applied in this manner.
// The returned configuration will be the new configuration with any auto-config settings
// already applied. If AutoConfig is not enabled this method will just parse any
// local configuration and return the built runtime configuration.
//
// The context passed in can be used to cancel the retrieval of the initial configuration
// like when receiving a signal during startup.
func (ac *AutoConfig) InitialConfiguration(ctx context.Context) (*config.RuntimeConfig, error) {
	if ac.config == nil {
		config, err := ac.ReadConfig()
		if err != nil {
			return nil, err
		}

		ac.config = config
	}

	switch {
	case ac.config.AutoConfig.Enabled:
		resp, err := ac.readPersistedAutoConfig()
		if err != nil {
			return nil, err
		}

		if resp == nil {
			ac.logger.Info("retrieving initial agent auto configuration remotely")
			resp, err = ac.getInitialConfiguration(ctx)
			if err != nil {
				return nil, err
			}
		}

		ac.logger.Debug("updating auto-config settings")
		if err = ac.recordInitialResponse(resp); err != nil {
			return nil, err
		}

		// re-read the configuration now that we have our initial auto-config
		config, err := ac.ReadConfig()
		if err != nil {
			return nil, err
		}

		ac.config = config
		return ac.config, nil
	case ac.config.AutoEncryptTLS:
		certs, err := ac.autoEncryptInitialCerts(ctx)
		if err != nil {
			return nil, err
		}

		if err := ac.setInitialTLSCertificates(certs); err != nil {
			return nil, err
		}

		ac.logger.Info("automatically upgraded to TLS")
		return ac.config, nil
	default:
		return ac.config, nil
	}
}

// introToken is responsible for determining the correct intro token to use
// when making the initial AutoConfig.InitialConfiguration RPC request.
func (ac *AutoConfig) introToken() (string, error) {
	conf := ac.config.AutoConfig
	// without an intro token or intro token file we cannot do anything
	if conf.IntroToken == "" && conf.IntroTokenFile == "" {
		return "", fmt.Errorf("neither intro_token or intro_token_file settings are not configured")
	}

	token := conf.IntroToken
	if token == "" {
		// load the intro token from the file
		content, err := ioutil.ReadFile(conf.IntroTokenFile)
		if err != nil {
			return "", fmt.Errorf("Failed to read intro token from file: %w", err)
		}

		token = string(content)

		if token == "" {
			return "", fmt.Errorf("intro_token_file did not contain any token")
		}
	}

	return token, nil
}

func (ac *AutoConfig) recordInitialResponse(resp *pbautoconf.AutoConfigResponse) error {
	signed, err := extractSignedResponse(resp)
	if err != nil {
		return fmt.Errorf("failed to extract certificates from the auto-config response: %w", err)
	}

	// we only do
	if err = ac.populateCertificateCache(signed); err != nil {
		return fmt.Errorf("failed to populate the cache with certificate responses: %w", err)
	}

	return ac.recordResponse(resp)
}

// recordResponse takes an AutoConfig RPC response records it with the agent
// This will persist the configuration to disk (unless in dev mode running without
// a data dir) and will reload the configuration.
func (ac *AutoConfig) recordResponse(resp *pbautoconf.AutoConfigResponse) error {
	if err := ac.update(resp); err != nil {
		return err
	}

	return ac.persistAutoConfig(resp)
}

// getInitialConfigurationOnce will perform full server to TCPAddr resolution and
// loop through each host trying to make the AutoConfig.InitialConfiguration RPC call. When
// successful the bool return will be true and the err value will indicate whether we
// successfully recorded the auto config settings (persisted to disk and stored internally
// on the AutoConfig object)
func (ac *AutoConfig) getInitialConfigurationOnce(ctx context.Context, csr string, key string) (*pbautoconf.AutoConfigResponse, error) {
	token, err := ac.introToken()
	if err != nil {
		return nil, err
	}

	request := pbautoconf.AutoConfigRequest{
		Datacenter: ac.config.Datacenter,
		Node:       ac.config.NodeName,
		Segment:    ac.config.SegmentName,
		JWT:        token,
		CSR:        csr,
	}

	var resp pbautoconf.AutoConfigResponse

	servers, err := ac.autoConfigHosts()
	if err != nil {
		return nil, err
	}

	for _, s := range servers {
		// try each IP to see if we can successfully make the request
		for _, addr := range ac.resolveHost(s) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			ac.logger.Debug("making AutoConfig.InitialConfiguration RPC", "addr", addr.String())
			if err = ac.acConfig.DirectRPC.RPC(ac.config.Datacenter, ac.config.NodeName, &addr, "AutoConfig.InitialConfiguration", &request, &resp); err != nil {
				ac.logger.Error("AutoConfig.InitialConfiguration RPC failed", "addr", addr.String(), "error", err)
				continue
			}
			ac.logger.Debug("AutoConfig.InitialConfiguration RPC was successful")

			// update the Certificate with the private key we generated locally
			if resp.Certificate != nil {
				resp.Certificate.PrivateKeyPEM = key
			}

			return &resp, nil
		}
	}

	return nil, fmt.Errorf("No servers successfully responded to the auto-config request")
}

// getInitialConfiguration implements a loop to retry calls to getInitialConfigurationOnce.
// It uses the RetryWaiter on the AutoConfig object to control how often to attempt
// the initial configuration process. It is also canceallable by cancelling the provided context.
func (ac *AutoConfig) getInitialConfiguration(ctx context.Context) (*pbautoconf.AutoConfigResponse, error) {
	// generate a CSR
	csr, key, err := ac.generateCSR()
	if err != nil {
		return nil, err
	}

	// this resets the failures so that we will perform immediate request
	wait := ac.acConfig.Waiter.Success()
	for {
		select {
		case <-wait:
			if resp, err := ac.getInitialConfigurationOnce(ctx, csr, key); err == nil && resp != nil {
				return resp, nil
			} else if err != nil {
				ac.logger.Error(err.Error())
			} else {
				ac.logger.Error("No error returned when fetching configuration from the servers but no response was either")
			}

			wait = ac.acConfig.Waiter.Failed()
		case <-ctx.Done():
			ac.logger.Info("interrupted during initial auto configuration", "err", ctx.Err())
			return nil, ctx.Err()
		}
	}
}

// update will take an AutoConfigResponse and do all things necessary
// to restore those settings. This currently involves updating the
// config data to be used during a call to ReadConfig, updating the
// tls Configurator and prepopulating the cache.
func (ac *AutoConfig) update(resp *pbautoconf.AutoConfigResponse) error {
	ac.autoConfigResponse = resp

	ac.autoConfigSource = config.LiteralSource{
		Name:   autoConfigFileName,
		Config: translateConfig(resp.Config),
	}

	if err := ac.updateTLSFromResponse(resp); err != nil {
		return err
	}

	return nil
}

func (ac *AutoConfig) Start(ctx context.Context) error {
	ac.Lock()
	defer ac.Unlock()

	if !ac.config.AutoConfig.Enabled && !ac.config.AutoEncryptTLS {
		return nil
	}

	if ac.running || ac.cancel != nil {
		return fmt.Errorf("AutoConfig is already running")
	}

	// create the top level context to control the go
	// routine executing the `run` method
	ctx, cancel := context.WithCancel(ctx)

	// create the channel to get cache update events through
	// really we should only ever get 10 updates
	ac.cacheUpdates = make(chan cache.UpdateEvent, 10)

	// setup the cache watches
	cancelCertWatches, err := ac.setupCertificateCacheWatches(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("error setting up cache watches: %w", err)
	}

	// start the token update notifier
	ac.tokenUpdates = ac.acConfig.Tokens.Notify(token.TokenKindAgent)

	// store the cancel funcs
	ac.cancel = cancel
	ac.cancelWatches = cancelCertWatches

	ac.running = true
	ac.done = make(chan struct{})
	go ac.run(ctx, ac.done)

	ac.logger.Info("auto-config started")
	return nil
}

func (ac *AutoConfig) Done() <-chan struct{} {
	ac.Lock()
	defer ac.Unlock()

	if ac.done != nil {
		return ac.done
	}

	// return a closed channel to indicate that we are already done
	done := make(chan struct{})
	close(done)
	return done
}

func (ac *AutoConfig) IsRunning() bool {
	ac.Lock()
	defer ac.Unlock()
	return ac.running
}

func (ac *AutoConfig) Stop() bool {
	ac.Lock()
	defer ac.Unlock()

	if !ac.running {
		return false
	}

	if ac.cancel != nil {
		ac.cancel()
	}

	return true
}
