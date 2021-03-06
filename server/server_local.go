// +build !clustered,!gcloud

/*
	This file supports opening and managing HTTP/RPC servers locally from one process
	instead of using always available services like in a cluster or Google cloud.  It
	also manages local or embedded storage engines.
*/

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/rpc"
	"github.com/janelia-flyem/dvid/storage"

	"github.com/janelia-flyem/go/toml"
)

const (
	// DefaultWebAddress is the default URL of the DVID web server
	DefaultWebAddress = "localhost:8000"

	// DefaultRPCAddress is the default RPC address for command-line use of a remote DVID server
	DefaultRPCAddress = "localhost:8001"

	// ErrorLogFilename is the name of the server error log, stored in the datastore directory.
	ErrorLogFilename = "dvid-errors.log"
)

var (
	// DefaultHost is the default most understandable alias for this server.
	DefaultHost = "localhost"

	tc tomlConfig
)

func init() {
	// Set default Host name for understandability from user perspective.
	// Assumes Linux or Mac.  From stackoverflow suggestion.
	cmd := exec.Command("/bin/hostname", "-f")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		dvid.Errorf("Unable to get default Host name via /bin/hostname: %v\n", err)
		dvid.Errorf("Using 'localhost' as default Host name.\n")
		return
	}
	DefaultHost = out.String()
	DefaultHost = DefaultHost[:len(DefaultHost)-1] // removes EOL
}

// TestConfig specifies configuration for testing servers.
type TestConfig struct {
	KVStoresMap  storage.DataMap
	LogStoresMap storage.DataMap
	CacheSize    map[string]int // MB for caches
}

// OpenTest initializes the server for testing, setting up caching, datastore, etc.
// Later configurations will override earlier ones.
func OpenTest(configs ...TestConfig) error {
	var dataMap datastore.DataStorageMap
	var dataMapped bool
	tc = tomlConfig{}
	if len(configs) > 0 {
		for _, c := range configs {
			if len(c.KVStoresMap) != 0 {
				dataMap.KVStores = c.KVStoresMap
				dataMapped = true
			}
			if len(c.LogStoresMap) != 0 {
				dataMap.LogStores = c.LogStoresMap
				dataMapped = true
			}
			if len(c.CacheSize) != 0 {
				for id, size := range c.CacheSize {
					if tc.Cache == nil {
						tc.Cache = make(map[string]sizeConfig)
					}
					tc.Cache[id] = sizeConfig{Size: size}
				}
			}
		}
	}
	config = &tc
	dvid.Infof("OpenTest with %v: cache setting %v\n", configs, tc.Cache)
	if dataMapped {
		datastore.OpenTest(dataMap)
	} else {
		datastore.OpenTest()
	}
	return nil
}

// CloseTest shuts down server for testing.
func CloseTest() {
	datastore.CloseTest()
}

type tomlConfig struct {
	Server     ServerConfig
	Email      dvid.EmailConfig
	Logging    dvid.LogConfig
	Kafka      storage.KafkaConfig
	Store      map[storage.Alias]storeConfig
	Backend    map[dvid.DataSpecifier]backendConfig
	Cache      map[string]sizeConfig
	Groupcache storage.GroupcacheConfig
}

// Some settings in the TOML can be given as relative paths.
// This function converts them in-place to absolute paths,
// assuming the given paths were relative to the TOML file's own directory.
func (c *tomlConfig) ConvertPathsToAbsolute(configPath string) error {
	var err error

	configDir := filepath.Dir(configPath)

	// [server].webClient
	c.Server.WebClient, err = dvid.ConvertToAbsolute(c.Server.WebClient, configDir)
	if err != nil {
		return fmt.Errorf("Error converting webClient setting to absolute path")
	}

	// [logging].logfile
	c.Logging.Logfile, err = dvid.ConvertToAbsolute(c.Logging.Logfile, configDir)
	if err != nil {
		return fmt.Errorf("Error converting logfile setting to absolute path")
	}

	// [store.foobar].path
	for alias, sc := range c.Store {
		p, ok := sc["path"]
		if !ok {
			continue
		}
		path, ok := p.(string)
		if !ok {
			return fmt.Errorf("Don't understand path setting for store %q", alias)
		}
		absPath, err := dvid.ConvertToAbsolute(path, configDir)
		if err != nil {
			return fmt.Errorf("Error converting store.%s.path to absolute path: %q", alias, path)
		}
		sc["path"] = absPath
	}
	return nil
}

func (c tomlConfig) Stores() (map[storage.Alias]dvid.StoreConfig, error) {
	stores := make(map[storage.Alias]dvid.StoreConfig, len(c.Store))
	for alias, sc := range c.Store {
		e, ok := sc["engine"]
		if !ok {
			return nil, fmt.Errorf("store configurations must have %q set to valid driver", "engine")
		}
		engine, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("engine set for store %q must be a string", alias)
		}
		var config dvid.Config
		config.SetAll(sc)
		stores[alias] = dvid.StoreConfig{
			Config: config,
			Engine: engine,
		}
	}
	return stores, nil
}

// Host returns the most understandable host alias + any port.
func (c *tomlConfig) Host() string {
	parts := strings.Split(c.Server.HTTPAddress, ":")
	host := c.Server.Host
	if len(parts) > 1 {
		host = host + ":" + parts[len(parts)-1]
	}
	return host
}

func (c *tomlConfig) Note() string {
	return c.Server.Note
}

func (c *tomlConfig) HTTPAddress() string {
	return c.Server.HTTPAddress
}

func (c *tomlConfig) RPCAddress() string {
	return c.Server.RPCAddress
}

func (c *tomlConfig) WebClient() string {
	return c.Server.WebClient
}

func (c *tomlConfig) AllowTiming() bool {
	return c.Server.AllowTiming
}

func (c *tomlConfig) KafkaServers() []string {
	if len(c.Kafka.Servers) != 0 {
		return c.Kafka.Servers
	}
	return nil
}

// CacheSize returns the number oF bytes reserved for the given identifier.
// If unset, will return 0.
func CacheSize(id string) int {
	if tc.Cache == nil {
		return 0
	}
	setting, found := tc.Cache[id]
	if !found {
		return 0
	}
	return setting.Size * dvid.Mega
}

// ServerConfig holds ports, host name, and other properties of this dvid server.
type ServerConfig struct {
	Host        string
	HTTPAddress string
	RPCAddress  string
	WebClient   string
	Note        string

	AllowTiming  bool   // If true, returns * for Timing-Allow-Origin in response headers.
	StartWebhook string // http address that should be called when server is started up.
	StartJaneliaConfig string // like StartWebhook, but with Janelia-specific behavior

	IIDGen   string `toml:"instance_id_gen"`
	IIDStart uint32 `toml:"instance_id_start"`
}

// DatastoreInstanceConfig returns data instance configuration necessary to
// handle id generation.
func (sc ServerConfig) DatastoreInstanceConfig() datastore.InstanceConfig {
	return datastore.InstanceConfig{
		Gen:   sc.IIDGen,
		Start: dvid.InstanceID(sc.IIDStart),
	}
}

// Initialize POSTs data to any set webhook indicating the server configuration.
func (sc ServerConfig) Initialize() error {
	if sc.StartWebhook == "" && sc.StartJaneliaConfig == "" {
		return nil
	}

	data := map[string]string{
		"Host":         sc.Host,
		"Note":         sc.Note,
		"HTTP Address": sc.HTTPAddress,
		"RPC Address":  sc.RPCAddress,
	}
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}

	if sc.StartWebhook != "" {
		req, err := http.NewRequest("POST", sc.StartWebhook, bytes.NewBuffer(jsonBytes))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("called webhook specified in TOML (%q) and received bad status code: %d", sc.StartWebhook, resp.StatusCode)
		}
	}

	if sc.StartJaneliaConfig != "" {
		// Janelia specific startup webhook; this format matches what's expected
		//	by our local config server
		// new: format like config server wants
		resp, err := http.PostForm(sc.StartJaneliaConfig, url.Values{"config": {string(jsonBytes)}})
		if err != nil {
		    return err
		}
		if resp.StatusCode != http.StatusOK {
		    return fmt.Errorf("called webhook specified in TOML (%q) and received bad status code: %d", sc.StartWebhook, resp.StatusCode)
		}
	}
	return nil
}

type sizeConfig struct {
	Size int
}

type storeConfig map[string]interface{}

type backendConfig struct {
	Store storage.Alias
	Log   storage.Alias
}

// LoadConfig loads DVID server configuration from a TOML file.
func LoadConfig(filename string) (*tomlConfig, *storage.Backend, error) {
	if filename == "" {
		return &tc, nil, fmt.Errorf("No server TOML configuration file provided")
	}
	if _, err := toml.DecodeFile(filename, &tc); err != nil {
		return &tc, nil, fmt.Errorf("could not decode TOML config: %v", err)
	}
	var err error
	err = tc.ConvertPathsToAbsolute(filename)
	if err != nil {
		return &tc, nil, fmt.Errorf("could not convert relative paths to absolute paths in TOML config: %v", err)
	}

	if tc.Email.IsAvailable() {
		dvid.SetEmailServer(tc.Email)
	}

	// Get all defined stores.
	backend := new(storage.Backend)
	backend.Groupcache = tc.Groupcache
	backend.Stores, err = tc.Stores()
	if err != nil {
		return &tc, nil, err
	}

	// Get default store if there's only one store defined.
	if len(backend.Stores) == 1 {
		for k := range backend.Stores {
			backend.DefaultKVDB = storage.Alias(strings.Trim(string(k), "\""))
		}
	}

	// Create the backend mapping.
	backend.KVStore = make(storage.DataMap)
	backend.LogStore = make(storage.DataMap)
	for k, v := range tc.Backend {
		// lookup store config
		_, found := backend.Stores[v.Store]
		if !found {
			return &tc, nil, fmt.Errorf("Backend for %q specifies unknown store %q", k, v.Store)
		}
		spec := dvid.DataSpecifier(strings.Trim(string(k), "\""))
		backend.KVStore[spec] = v.Store
		dvid.Infof("backend.KVStore[%s] = %s\n", spec, v.Store)
		if v.Log != "" {
			backend.LogStore[spec] = v.Log
		}
	}
	defaultStore, found := backend.KVStore["default"]
	if found {
		backend.DefaultKVDB = defaultStore
	} else {
		if backend.DefaultKVDB == "" {
			return &tc, nil, fmt.Errorf("if no default backend specified, must have exactly one store defined in config file")
		}
	}

	defaultLog, found := backend.LogStore["default"]
	if found {
		backend.DefaultLog = defaultLog
	}

	defaultMetadataName, found := backend.KVStore["metadata"]
	if found {
		backend.Metadata = defaultMetadataName
	} else {
		if backend.DefaultKVDB == "" {
			return &tc, nil, fmt.Errorf("can't set metadata if no default backend specified, must have exactly one store defined in config file")
		}
		backend.Metadata = backend.DefaultKVDB
	}

	// The server config could be local, cluster, gcloud-specific config.  Here it is local.
	config = &tc
	return &tc, backend, nil
}

// Serve starts HTTP and RPC servers.
func Serve() {
	// Use defaults if not set via TOML config file.
	if tc.Server.Host == "" {
		tc.Server.Host = DefaultHost
	}
	if tc.Server.HTTPAddress == "" {
		tc.Server.HTTPAddress = DefaultWebAddress
	}
	if tc.Server.RPCAddress == "" {
		tc.Server.RPCAddress = DefaultRPCAddress
	}

	dvid.Infof("------------------\n")
	dvid.Infof("DVID code version: %s\n", gitVersion)
	dvid.Infof("Serving HTTP on %s (host alias %q)\n", tc.Server.HTTPAddress, tc.Server.Host)
	dvid.Infof("Serving command-line use via RPC %s\n", tc.Server.RPCAddress)
	dvid.Infof("Using web client files from %s\n", tc.Server.WebClient)
	dvid.Infof("Using %d of %d logical CPUs for DVID.\n", dvid.NumCPU, runtime.NumCPU())

	// Launch the web server
	go serveHTTP()

	// Launch the rpc server
	go func() {
		if err := rpc.StartServer(tc.Server.RPCAddress); err != nil {
			dvid.Criticalf("Could not start RPC server: %v\n", err)
		}
	}()

	<-shutdownCh
}
