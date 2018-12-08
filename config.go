package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v2"
)

var defaultConfigContent = `
Service:
  Listen: "127.0.0.1:8000"
  SignKey: ""
  SignTTL: 300
  SignDisabled: false
  MaxContentLength: 4194304
  RedisStoreAddress: "127.0.0.1:6379"
  CookieKey: ""
  TLSEnabled: false
  TLSCertFile: ""
  TLSKeyFile: ""

Datalab:
  HookScriptMaxExecutionSeconds: 300
  HookAfterStart: |-
    /usr/bin/env
    python3
    ./scripts/hook_after_start.py
    ${datalab_home}
    ${default_filename}

  DataHome: "./data"
  DataBackupHome: "./backup"
  BasePort: 10000
  NonceTimeout: 600
  MaxInstanceCount: 20
  RedisCacheURL: "redis://127.0.0.1:6379/1"
  DockerEndpoint: "unix:///var/run/docker.sock"
  InstanceAutoRemove: true
  # browser open, but no interaction, after timeout expired, close instance
  InstanceIdleTimeout: 1800
  # browser closed, after timeout expired, close instance
  InstanceKeepAliveTimeout: 300

`

type Config struct {
	Service ServiceConfig
	Datalab DatalabConfig
}

type ServiceConfig struct {
	Listen            string
	SignKey           string
	SignTTL           int
	SignDisabled      bool
	MaxContentLength  int64
	RedisStoreAddress string
	CookieKey         string
	TLSEnabled        bool
	TLSCertFile       string
	TLSKeyFile        string
}

type DatalabConfig struct {
	HookScriptMaxExecutionSeconds int
	HookAfterStart                string
	DataHome                      string
	DataBackupHome                string
	BasePort                      int
	NonceTimeout                  int
	MaxInstanceCount              int
	RedisCacheURL                 string
	DockerEndpoint                string
	InstanceAutoRemove            bool
	InstanceIdleTimeout           int // browser open, but no interaction, after timeout expired, close instance
	InstanceKeepAliveTimeout      int // browser closed, after timeout expired, close instance
}

func setupConfig(configFilename string) (config *Config, err error) {
	var (
		configPath string
		configName string
		configExt  string
	)

	if configFilename == "" {
		configFilename = "./config/default.yml"
	}

	configName = filepath.Base(configFilename)
	configExt = filepath.Ext(configName)
	configName = configName[0 : len(configName)-len(configExt)]
	configPath = filepath.Dir(configFilename)
	log.Printf("Loading config from directory: %s, config name: %s", configPath, configName)

	// check file exists
	saveDefaultConfigIfNotExists(configPath, configName, configExt)

	viper.SetConfigName(configName)
	viper.AddConfigPath(configPath)

	if err = viper.ReadInConfig(); err != nil {
		err = fmt.Errorf("read config error: %v", err)
		return
	}

	config = &Config{}
	if err = viper.Unmarshal(config); err != nil {
		err = fmt.Errorf("unmarshal config error: %v", err)
		return
	}

	content, _ := yaml.Marshal(config)
	log.Printf("config loaded:\n%v", string(content))

	if err = checkConfig(config); err != nil {
		return
	}

	return
}

func checkConfig(config *Config) (err error) {
	// service config

	if config.Service.CookieKey == "" {
		err = fmt.Errorf("configuration: Service.CookieKey should not be empty")
		return
	}

	if config.Service.SignKey == "" {
		err = fmt.Errorf("configuration: Service.SignKey should not be empty")
		return
	}

	if config.Service.SignTTL <= 0 {
		config.Service.SignTTL = 300
	}

	if config.Service.TLSEnabled {
		if config.Service.TLSCertFile == "" {
			err = fmt.Errorf("configuration: Service.TLSCertFile should not be empty if TLSEnabled = true")
			return
		}
		if config.Service.TLSKeyFile == "" {
			err = fmt.Errorf("configuration: Service.TLSKeyFile should not be empty if TLSEnabled = true")
			return
		}
	}

	// datalab config

	if config.Datalab.InstanceIdleTimeout <= 0 {
		config.Datalab.InstanceIdleTimeout = 1800
	}
	if config.Datalab.InstanceKeepAliveTimeout <= 0 {
		config.Datalab.InstanceKeepAliveTimeout = 300
  }
  
  if config.Datalab.HookScriptMaxExecutionSeconds <= 0 {
    config.Datalab.HookScriptMaxExecutionSeconds = 300
  }

	return
}

func saveDefaultConfigIfNotExists(configPath string, configName string, configExt string) (err error) {
	var configFullPath string
	configFullPath = filepath.Join(configPath, configName+configExt)
	_, err = os.Stat(configFullPath)
	if err == nil {
		return
	}
	if os.IsNotExist(err) {
		if err = os.MkdirAll(configPath, 0755); err != nil {
			log.Printf("create config directory failure: %s, err = %v", configPath, err)
			return
		}
		var fout *os.File
		if fout, err = os.Create(configFullPath); err != nil {
			log.Printf("create config file failure: %s, err = %v", configFullPath, err)
			return
		}
		defer fout.Close()
		fout.WriteString(defaultConfigContent)
		log.Printf("creating default config file: %s", configFullPath)
	}
	return
}
