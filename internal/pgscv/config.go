package pgscv

import (
	"fmt"
	"github.com/jackc/pgx/v4"
	"github.com/weaponry/pgscv/internal/filter"
	"github.com/weaponry/pgscv/internal/log"
	"github.com/weaponry/pgscv/internal/service"
	"gopkg.in/yaml.v2"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultListenAddress     = "127.0.0.1:9890"
	defaultPostgresUsername  = "pgscv"
	defaultPostgresDbname    = "postgres"
	defaultPgbouncerUsername = "pgscv"
	defaultPgbouncerDbname   = "pgbouncer"

	defaultSendMetricsInterval = 60 * time.Second
)

// Config defines application's configuration.
type Config struct {
	BinaryPath           string                // full path of the program, required for auto-update procedure
	BinaryVersion        string                // version of the program, required for auto-update procedure
	AutoUpdate           bool                  `yaml:"autoupdate"`       // control auto-update enabled or not
	NoTrackMode          bool                  `yaml:"no_track_mode"`    // controls tracking sensitive information (query texts, etc)
	ListenAddress        string                `yaml:"listen_address"`   // Network address and port where the application should listen on
	SendMetricsURL       string                `yaml:"send_metrics_url"` // URL of Weaponry service metric gateway
	SendMetricsInterval  time.Duration         // Metric send interval
	APIKey               string                `yaml:"api_key"`  // API key for accessing to Weaponry
	ServicesConnSettings []service.ConnSetting `yaml:"services"` // Slice of connection settings for exact services
	Defaults             map[string]string     `yaml:"defaults"` // Defaults
	Filters              filter.Filters        `yaml:"filters"`
	DisableCollectors    []string              `yaml:"disable_collectors"` // List of collectors which should be disabled.
}

// NewConfig creates new config based on config file or return default config of config is not exists.
func NewConfig(configFilePath string) (*Config, error) {
	if configFilePath == "" {
		return &Config{Defaults: map[string]string{}}, nil
	}

	content, err := os.ReadFile(filepath.Clean(configFilePath))
	if err != nil {
		return nil, err
	}

	config := Config{Defaults: map[string]string{}}
	err = yaml.Unmarshal(content, &config)
	if err != nil {
		return nil, err
	}

	log.Infoln("read configuration from ", configFilePath)
	return &config, nil
}

// Validate checks configuration for stupid values and set defaults
func (c *Config) Validate() error {
	c.SendMetricsInterval = defaultSendMetricsInterval

	// API key is necessary when Metric Service is specified
	if c.SendMetricsURL != "" && c.APIKey == "" {
		return fmt.Errorf("API key should be specified")
	}

	if c.ListenAddress == "" {
		c.ListenAddress = defaultListenAddress
	}

	log.Infoln("*** IMPORTANT ***: pgSCV by default collects information about user queries. Tracking queries can be disabled with 'no_track_mode: true' in config file.")
	if c.NoTrackMode {
		log.Infoln("no-track mode enabled: tracking disabled for [pg_stat_statements.query].")
	} else {
		log.Infoln("no-track mode disabled")
	}

	// setup defaults
	if c.Defaults == nil {
		c.Defaults = map[string]string{}
	}

	if _, ok := c.Defaults["postgres_username"]; !ok {
		c.Defaults["postgres_username"] = defaultPostgresUsername
	}

	if _, ok := c.Defaults["postgres_dbname"]; !ok {
		c.Defaults["postgres_dbname"] = defaultPostgresDbname
	}

	if _, ok := c.Defaults["pgbouncer_username"]; !ok {
		c.Defaults["pgbouncer_username"] = defaultPgbouncerUsername
	}

	if _, ok := c.Defaults["pgbouncer_dbname"]; !ok {
		c.Defaults["pgbouncer_dbname"] = defaultPgbouncerDbname
	}

	// User might specify its own set of services which he would like to monitor. This services should be validated and
	// invalid should be rejected. Validation is performed using pgx.ParseConfig method which does all dirty work.
	if c.ServicesConnSettings != nil {
		if len(c.ServicesConnSettings) != 0 {
			for _, s := range c.ServicesConnSettings {
				if s.ServiceType == "" {
					return fmt.Errorf("service_type is not specified for %s", s.Conninfo)
				}

				_, err := pgx.ParseConfig(s.Conninfo)
				if err != nil {
					return fmt.Errorf("invalid conninfo: %s", err)
				}
			}
		}
	}

	// Add default filters and compile regexps.
	if c.Filters == nil {
		c.Filters = filter.New()
	}
	c.Filters.SetDefault()
	if err := c.Filters.Compile(); err != nil {
		return err
	}

	return nil
}
