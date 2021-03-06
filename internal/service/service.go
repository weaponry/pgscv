package service

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"github.com/jackc/pgx/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/shirou/gopsutil/process"
	"github.com/weaponry/pgscv/internal/collector"
	"github.com/weaponry/pgscv/internal/filter"
	"github.com/weaponry/pgscv/internal/log"
	"github.com/weaponry/pgscv/internal/model"
	"github.com/weaponry/pgscv/internal/store"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultHost              = "127.0.0.1"
	defaultPgbouncerPort     = 6432
	defaultPostgresUsername  = "pgscv"
	defaultPostgresDbname    = "postgres"
	defaultPgbouncerUsername = "pgscv"
	defaultPgbouncerDbname   = "pgbouncer"
)

// Service struct describes service - the target from which should be collected metrics.
type Service struct {
	// Service identifier is unique key across all monitored services and used to distinguish services of the same type
	// running on the single host (two Postgres services running on the same host but listening on different ports).
	// Hence not to mix their metrics the ServiceID is introduced and attached to metrics as "sid" label:
	// metric_xact_commits{database="test", sid="postgres:5432"} -- metric from the first postgres running on 5432 port
	// metric_xact_commits{database="test", sid="postgres:5433"} -- metric from the second postgres running on 5433 port
	ServiceID string
	// Connection settings required for connecting to the service.
	ConnSettings ConnSetting
	// Prometheus-based metrics collector associated with the service. Each 'service' has its own dedicated collector instance
	// which implements a service-specific set of metric collectors.
	Collector Collector
	// TotalErrors represents total number of times where service's health checks failed. When errors limit is reached service
	// removed from the repo.
	TotalErrors int
}

// Config defines service's configuration.
type Config struct {
	RuntimeMode        int
	NoTrackMode        bool
	ConnDefaults       map[string]string `yaml:"defaults"` // Defaults
	ConnSettings       []ConnSetting
	Filters            map[string]filter.Filter
	DisabledCollectors []string
}

// Exporter is an interface for prometheus.Collector.
type Collector interface {
	Describe(chan<- *prometheus.Desc)
	Collect(chan<- prometheus.Metric)
}

// ConnSetting describes connection settings required for connecting to particular service. This struct primarily
// is used for representing services defined by user in the config file.
type ConnSetting struct {
	// ServiceType defines type of service for which these connection settings are used.
	ServiceType string `yaml:"service_type"`
	// Conninfo is the connection string in service-specific format.
	Conninfo string `yaml:"conninfo"`
}

// connectionParams is the set of parameters that may be required when constructing connection string.
// For example, this struct describes the postmaster.pid representation https://www.postgresql.org/docs/current/storage-file-layout.html
type connectionParams struct {
	pid               int    // process id
	datadirPath       string // instance data directory path
	startTs           int64  // postmaster start timestamp
	unixSocketDirPath string // UNIX-domain socket directory path
	listenAddr        string // first valid listen_address (IP address or *, or empty if not listening on TCP)
	listenPort        int    // port number
	// ... other stuff we're not interested in
}

// Repository is the repository with services.
type Repository struct {
	sync.RWMutex                    // protect concurrent access
	Services     map[string]Service // service repo store
}

// NewRepository creates new services repository.
func NewRepository() *Repository {
	return &Repository{
		Services: make(map[string]Service),
	}
}

/* Public wrapper-methods of Repository */

//
func (repo *Repository) GetService(id string) Service {
	return repo.getService(id)
}

//
func (repo *Repository) TotalServices() int {
	return repo.totalServices()
}

//
func (repo *Repository) GetServiceIDs() []string {
	return repo.getServiceIDs()
}

//
func (repo *Repository) AddServicesFromConfig(config Config) {
	repo.addServicesFromConfig(config)
}

//
func (repo *Repository) SetupServices(config Config) error {
	return repo.setupServices(config)
}

//
func (repo *Repository) StartBackgroundDiscovery(ctx context.Context, config Config) {
	repo.startBackgroundDiscovery(ctx, config)
}

/* Private methods of Repository */

// addService adds service to the repo.
func (repo *Repository) addService(id string, s Service) {
	repo.Lock()
	repo.Services[id] = s
	repo.Unlock()
}

// getService returns the service from repo with specified ID.
func (repo *Repository) getService(id string) Service {
	repo.RLock()
	s := repo.Services[id]
	repo.RUnlock()
	return s
}

// markServiceFailed increments total number of health check errors.
func (repo *Repository) markServiceFailed(id string) {
	repo.Lock()
	s := repo.Services[id]
	s.TotalErrors++
	repo.Services[id] = s
	repo.Unlock()
}

// getServiceStatus returns total number of errors (failed health checks).
func (repo *Repository) getServiceStatus(id string) int {
	repo.RLock()
	n := repo.Services[id].TotalErrors
	repo.RUnlock()
	return n
}

// markServiceHealthy resets health check errors counter to zero.
func (repo *Repository) markServiceHealthy(id string) {
	repo.Lock()
	s := repo.Services[id]
	s.TotalErrors = 0
	repo.Services[id] = s
	repo.Unlock()
}

// removeService removes service from the repo.
func (repo *Repository) removeService(id string) {
	repo.Lock()
	delete(repo.Services, id)
	repo.Unlock()
}

// totalServices returns the number of services registered in the repo.
func (repo *Repository) totalServices() int {
	repo.RLock()
	var size = len(repo.Services)
	repo.RUnlock()
	return size
}

// getServiceIDs returns slice of services' IDs in the repo.
func (repo *Repository) getServiceIDs() []string {
	var serviceIDs = make([]string, 0, repo.totalServices())
	repo.RLock()
	for i := range repo.Services {
		serviceIDs = append(serviceIDs, i)
	}
	repo.RUnlock()
	return serviceIDs
}

// addServicesFromConfig reads info about services from the config file and fulfill the repo.
func (repo *Repository) addServicesFromConfig(config Config) {
	log.Debug("config: add services from config file")

	// Always add system service.
	repo.addService("system:0", Service{ServiceID: "system:0", ConnSettings: ConnSetting{ServiceType: model.ServiceTypeSystem}})
	log.Info("registered new service [system:0]")

	// Sanity check, but basically should be always passed.
	if config.ConnSettings == nil {
		log.Warn("connection settings for service are not defined, do nothing")
		return
	}

	// Check all passed connection settings and try to connect using them. In case of success, create a 'Service' instance
	// in the repo.
	for _, cs := range config.ConnSettings {
		// *ConnConfig struct will be used for
		//   1) doing connection;
		//   2) getting connection properties to define service-specific parameters.
		pgconfig, err := pgx.ParseConfig(cs.Conninfo)
		if err != nil {
			log.Warnf("%s: %s, skip", cs.Conninfo, err)
			continue
		}

		// Check connection using created *ConnConfig, go next if connection failed.
		db, err := store.NewWithConfig(pgconfig)
		if err != nil {
			log.Warnf("%s: %s, skip", cs.Conninfo, err)
			continue
		}
		db.Close()

		// Connection was successful, create 'Service' struct with service-related properties and add it to service repo.
		s := Service{
			ServiceID:    cs.ServiceType + ":" + strconv.Itoa(int(pgconfig.Port)),
			ConnSettings: cs,
			Collector:    nil,
		}

		// Add "host", because user might manually specify services with the same port (but the are running on different hosts).
		var key = strings.Join([]string{cs.ServiceType, pgconfig.Host, strconv.Itoa(int(pgconfig.Port))}, ":")
		repo.addService(key, s)
		log.Infof("registered new service [%s]", s.ServiceID)
		log.Debugf("new service available through: %s@%s:%d/%s", pgconfig.User, pgconfig.Host, pgconfig.Port, pgconfig.Database)
	}
}

// startBackgroundDiscovery looking for services and add them to the repo.
func (repo *Repository) startBackgroundDiscovery(ctx context.Context, config Config) {
	log.Debug("starting background auto-discovery loop")

	// add pseudo-service for system metrics
	repo.addService("system:0", Service{ServiceID: "system:0", ConnSettings: ConnSetting{ServiceType: model.ServiceTypeSystem}})
	log.Infoln("auto-discovery: service added [system:0]")

	for {
		if err := repo.lookupServices(config); err != nil {
			log.Warnf("auto-discovery: services lookup failed: %s; skip", err)
			continue
		}
		if err := repo.setupServices(config); err != nil {
			log.Warnf("auto-discovery: services setup failed: %s; skip", err)
			continue
		}

		// Perform health check for services with remote endpoints (e.g. Postgres or Pgbouncer). Services which continuously
		// don't respond are removed from the repo (but if they appear later they will be discovered again).
		repo.healthcheckServices()

		// Sleep until timeout or exit if context canceled.
		select {
		case <-time.After(60 * time.Second):
			continue
		case <-ctx.Done():
			log.Info("auto-discovery: exit signaled")
			return
		}
	}
}

// lookupServices scans PIDs and looking for required services
func (repo *Repository) lookupServices(config Config) error {
	log.Debug("auto-discovery: looking up for new services...")

	pids, err := process.Pids()
	if err != nil {
		return err
	}

	// walk through the pid list and looking for the processes with appropriate names
	for _, pid := range pids {
		proc, err := process.NewProcess(pid)
		if err != nil {
			log.Debugf("auto-discovery: failed to create process struct for pid %d: %s; skip", pid, err)
			continue
		}

		name, err := proc.Name()
		if err != nil {
			log.Debugf("auto-discovery: no process name for pid %d: %s; skip", pid, err)
			continue // skip processes with no names
		}

		switch name {
		case "postgres":
			ppid, _ := proc.Ppid() // error doesn't matter here, even if ppid will be 0 - we're interested in ppid == 1
			if ppid == 1 {
				postgres, err := discoverPostgres(proc, config)
				if err != nil {
					log.Warnf("auto-discovery [postgres]: discovery failed: %s; skip", err)
					break
				}

				// check service in the repo
				if s := repo.getService(postgres.ServiceID); s.ServiceID == postgres.ServiceID {
					log.Debugf("auto-discovery [postgres]: service [%s] already in the repository, skip", s.ServiceID)
					break
				}

				repo.addService(postgres.ServiceID, postgres) // add postgresql service to the repo
				log.Infof("auto-discovery [postgres]: service added [%s]", postgres.ServiceID)
			}
		case "pgbouncer":
			pgbouncer, err := discoverPgbouncer(proc, config)
			if err != nil {
				log.Warnf("auto-discovery [pgbouncer]: discovery failed: %s; skip", err)
				break
			}

			// check service in the repo
			if s := repo.getService(pgbouncer.ServiceID); s.ServiceID == pgbouncer.ServiceID {
				log.Debugf("auto-discovery [pgbouncer]: service [%s] already in the repository, skip", s.ServiceID)
				break
			}

			repo.addService(pgbouncer.ServiceID, pgbouncer) // add pgbouncer service to the repo
			log.Infof("auto-discovery [pgbouncer]: service added [%s]", pgbouncer.ServiceID)
		default:
			continue // others are not interesting
		}
	}
	return nil
}

// setupServices attaches metrics exporters to the services in the repo.
func (repo *Repository) setupServices(config Config) error {
	log.Debug("config: setting up services")

	for _, id := range repo.getServiceIDs() {
		var service = repo.getService(id)
		if service.Collector == nil {
			factories := collector.Factories{}
			collectorConfig := collector.Config{
				NoTrackMode: config.NoTrackMode,
				ServiceType: service.ConnSettings.ServiceType,
				ConnString:  service.ConnSettings.Conninfo,
				Filters:     config.Filters,
			}

			switch service.ConnSettings.ServiceType {
			case model.ServiceTypeSystem:
				factories.RegisterSystemCollectors(config.DisabledCollectors)
			case model.ServiceTypePostgresql:
				factories.RegisterPostgresCollectors(config.DisabledCollectors)
				cfg, err := collector.NewPostgresServiceConfig(collectorConfig.ConnString)
				if err != nil {
					log.Errorf("service [%s] setup failed: %s; skip", service.ServiceID, err)
					continue
				}
				collectorConfig.PostgresServiceConfig = cfg
			case model.ServiceTypePgbouncer:
				factories.RegisterPgbouncerCollectors(config.DisabledCollectors)
			default:
				continue
			}

			mc, err := collector.NewPgscvCollector(service.ServiceID, factories, collectorConfig)
			if err != nil {
				return err
			}
			service.Collector = mc

			// Register collector.
			prometheus.MustRegister(service.Collector)

			// put updated service copy into repo
			repo.addService(id, service)
			log.Debugf("service configured [%s]", id)
		}
	}

	return nil
}

// healthcheckServices performs services health checks and remove those who don't respond too long
func (repo *Repository) healthcheckServices() {
	log.Debug("services healthcheck started")

	// Remove service after 10 failed health checks.
	var errorThreshold = 10

	for _, id := range repo.getServiceIDs() {
		var service = repo.getService(id)

		switch service.ConnSettings.ServiceType {
		case model.ServiceTypePostgresql, model.ServiceTypePgbouncer:
			totalErrors := repo.getServiceStatus(id)
			err := attemptConnect(service.ConnSettings.Conninfo)
			if err != nil {
				totalErrors++
				if totalErrors < errorThreshold {
					repo.markServiceFailed(id)
					log.Warnf("service [%s] failed: tries remain %d/%d", id, totalErrors, errorThreshold)
				} else {
					// unregister collector and remove the service.
					if repo.Services[id].Collector != nil {
						prometheus.Unregister(repo.Services[id].Collector)
					}

					repo.removeService(id)
					log.Errorf("service [%s] removed: too many failures %d/%d", id, totalErrors, errorThreshold)
				}
			}
		default:
			continue
		}
	}

	log.Debug("services healthcheck finished")
}

// discoverPostgres reads "datadir" argument from Postmaster's cmdline string and reads postmaster.pid stored in data
// directory. Using postmaster.pid data construct "conninfo" string and test it through making a connection.
func discoverPostgres(proc *process.Process, config Config) (Service, error) {
	log.Debugf("auto-discovery [postgres]: analyzing process with pid %d", proc.Pid)

	cmdline, err := proc.CmdlineSlice()
	if err != nil {
		return Service{}, err
	}
	// parse cmdline
	datadirCmdPath, err := parsePostgresProcessCmdline(cmdline)
	if err != nil {
		return Service{}, err
	}

	connParams, err := newPostgresConnectionParams(datadirCmdPath + "/postmaster.pid")
	if err != nil {
		return Service{}, err
	}

	// Construct the connection string using the data from postmaster.pid and user-defined defaults.
	// Depending on configured Postgres there can be UNIX-based or TCP-based connection string
	var connString string
	for _, v := range []bool{true, false} {
		connString = newPostgresConnectionString(connParams, config.ConnDefaults, v)
		if err := attemptConnect(connString); err == nil {
			// no need to continue because connection with created connString was successful
			break
		}
	}

	s := Service{
		ServiceID:    model.ServiceTypePostgresql + ":" + strconv.Itoa(connParams.listenPort),
		ConnSettings: ConnSetting{ServiceType: model.ServiceTypePostgresql, Conninfo: connString},
		Collector:    nil,
	}

	log.Debugf("auto-discovery [postgres]: service has been found, pid %d, available through %s", proc.Pid, connString)
	return s, nil
}

// parsePostgresProcessCmdline parses postgres process cmdline for data directory argument
func parsePostgresProcessCmdline(cmdline []string) (string, error) {
	for i, arg := range cmdline {
		if arg == "-D" && len(cmdline) > i+1 {
			return cmdline[i+1], nil
		}
	}
	return "", fmt.Errorf("data directory argument not found")
}

// newPostgresConnectionParams reads connection parameters from postmaster.pid
func newPostgresConnectionParams(pidFilePath string) (connectionParams, error) {
	p := connectionParams{}
	content, err := os.ReadFile(filepath.Clean(pidFilePath))
	if err != nil {
		return p, err
	}

	reader := bufio.NewReader(bytes.NewBuffer(content))
	for i := 0; ; i++ {
		line, _, err := reader.ReadLine()
		if err == io.EOF {
			break
		} else if err != nil {
			return p, err
		}
		switch i {
		case 0:
			p.pid, err = strconv.Atoi(string(line))
			if err != nil {
				return p, err
			}
		case 1:
			p.datadirPath = string(line)
		case 2:
			p.startTs, err = strconv.ParseInt(string(line), 10, 64)
			if err != nil {
				return p, err
			}
		case 3:
			p.listenPort, err = strconv.Atoi(string(line))
			if err != nil {
				return p, err
			}
		case 4:
			p.unixSocketDirPath = string(line)
		case 5:
			if string(line) == "*" {
				p.listenAddr = defaultHost
			} else {
				p.listenAddr = string(line)
			}
		}
	}
	return p, nil
}

// newPostgresConnectionString creates special connection string for connecting to Postgres using passed connection parameters.
func newPostgresConnectionString(connParams connectionParams, defaults map[string]string, unix bool) string {
	var password, connString string
	var username = defaultPostgresUsername
	var dbname = defaultPostgresDbname

	if _, ok := defaults["postgres_username"]; ok {
		username = defaults["postgres_username"]
	}

	if _, ok := defaults["postgres_dbname"]; ok {
		dbname = defaults["postgres_dbname"]
	}

	if _, ok := defaults["postgres_password"]; ok {
		password = defaults["postgres_password"]
	}

	connString = "application_name=pgscv"

	if unix && connParams.unixSocketDirPath != "" {
		connString = fmt.Sprintf("%s host=%s", connString, connParams.unixSocketDirPath)
	} else if !unix && connParams.listenAddr != "" {
		connString = fmt.Sprintf("%s host=%s", connString, connParams.listenAddr)
	}

	if connParams.listenPort > 0 {
		connString = fmt.Sprintf("%s port=%d", connString, connParams.listenPort)
	}

	connString = fmt.Sprintf("%s user=%s dbname=%s", connString, username, dbname)

	if password != "" {
		connString = fmt.Sprintf("%s password=%s", connString, password)
	}

	return connString
}

// discoverPgbouncer check passed process is it a Pgbouncer process or not.
func discoverPgbouncer(proc *process.Process, config Config) (Service, error) {
	log.Debugf("auto-discovery [pgbouncer]: analyzing process with pid %d", proc.Pid)

	cmdline, err := proc.Cmdline()
	if err != nil {
		return Service{}, err
	}

	if len(cmdline) == 0 {
		return Service{}, fmt.Errorf("empty cmdline")
	}

	// extract config file location from cmdline
	configFilePath := parsePgbouncerCmdline(cmdline)

	// parse ini file
	connParams, err := parsePgbouncerIniFile(configFilePath)
	if err != nil {
		return Service{}, err
	}

	connString := newPgbouncerConnectionString(connParams, config.ConnDefaults)

	if err := attemptConnect(connString); err != nil {
		return Service{}, err
	}

	s := Service{
		ServiceID:    model.ServiceTypePgbouncer + ":" + strconv.Itoa(connParams.listenPort),
		ConnSettings: ConnSetting{ServiceType: model.ServiceTypePgbouncer, Conninfo: connString},
		Collector:    nil,
	}

	log.Debugf("auto-discovery: pgbouncer service has been found, pid %d, available through %s:%d", proc.Pid, connParams.listenAddr, connParams.listenPort)
	return s, nil
}

// parsePgbouncerIniFile reads pgbouncer's config ini file and returns connection parameters required for constructing connection string.
func parsePgbouncerIniFile(iniFilePath string) (connectionParams, error) {
	// read the content of inifile
	file, err := os.Open(filepath.Clean(iniFilePath))
	if err != nil {
		return connectionParams{}, err
	}
	defer func() { _ = file.Close() }()

	var paramName, paramValue string
	var connParams connectionParams

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if err := scanner.Err(); err != nil {
			log.Warnln("an error occurred during scan: ", err)
			continue
		}
		line := scanner.Text()

		// skip comments and empty lines
		if strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") || len(line) == 0 {
			continue
		}

		line = strings.Replace(line, " ", "", -1)
		vals := strings.Split(line, "=")
		if len(vals) != 2 {
			// if parameter is not set it means default valus is used, can skip that line
			continue
		}

		// looking for listen address and port settings, use them as connection settings
		paramName, paramValue = vals[0], vals[1]
		switch paramName {
		case "listen_addr":
			connParams.listenAddr = strings.Split(paramValue, ",")[0] // take first address
			if connParams.listenAddr == "*" {
				connParams.listenAddr = defaultHost
			}
		case "listen_port":
			connParams.listenPort, err = strconv.Atoi(paramValue)
			if err != nil {
				return connectionParams{}, err
			}
		case "unix_socket_dir":
			connParams.unixSocketDirPath = paramValue
		}
	}

	// set defaults in case of empty values, for more details see pgbouncer.ini reference https://www.pgbouncer.org/config.html
	if connParams.unixSocketDirPath == "" {
		connParams.unixSocketDirPath = "/tmp"
	}
	if connParams.listenPort == 0 {
		connParams.listenPort = defaultPgbouncerPort
	}

	return connParams, nil
}

// newPgbouncerConnectionString creates special connection string for connecting to Pgbouncer using passed connection parameters.
func newPgbouncerConnectionString(connParams connectionParams, defaults map[string]string) string {
	var password, connString string
	var username = defaultPgbouncerUsername

	if _, ok := defaults["pgbouncer_username"]; ok {
		username = defaults["pgbouncer_username"]
	}

	if _, ok := defaults["pgbouncer_password"]; ok {
		password = defaults["pgbouncer_password"]
	}

	connString = "application_name=pgscv"

	if connParams.listenAddr != "" {
		connString = fmt.Sprintf("%s host=%s", connString, connParams.listenAddr)
	} else if connParams.unixSocketDirPath != "" {
		connString = fmt.Sprintf("%s host=%s", connString, connParams.unixSocketDirPath)
	}

	if connParams.listenPort > 0 {
		connString = fmt.Sprintf("%s port=%d", connString, connParams.listenPort)
	}

	connString = fmt.Sprintf("%s user=%s dbname=%s", connString, username, defaultPgbouncerDbname)

	if password != "" {
		connString = fmt.Sprintf("%s password=%s", connString, password)
	}

	return connString
}

// attemptConnect tries to make a real connection using passed connection string.
func attemptConnect(connString string) error {
	log.Debugln("making test connection: ", connString)
	db, err := store.New(connString)
	if err != nil {
		return err
	}

	db.Close()
	log.Debug("test connection success")

	return nil
}

// parsePgbouncerCmdline parses pgbouncer's cmdline and extract config file location.
func parsePgbouncerCmdline(cmdline string) string {
	parts := strings.Fields(cmdline)

	// For extracting config file from cmdline we should skip first argument (pgbouncer executable) and skip all arguments
	// which starting with '-' symbol. See test function for examples.

	for _, s := range parts[1:] {
		if !strings.HasPrefix(s, "-") {
			return s
		}
	}
	return ""
}

// stringsContains returns true if array of strings contains specific string
func stringsContains(ss []string, s string) bool {
	for _, val := range ss {
		if val == s {
			return true
		}
	}
	return false
}
