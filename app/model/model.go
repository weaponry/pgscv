package model

import "github.com/prometheus/client_golang/prometheus"

const (
	// ServiceTypeDisabled ...
	ServiceTypeDisabled = iota
	// ServiceTypePostgresql ...
	ServiceTypePostgresql
	// ServiceTypePgbouncer ...
	ServiceTypePgbouncer
	// ServiceTypeSystem ...
	ServiceTypeSystem
)

// Service describes discovered service
type Service struct {
	ServiceType int      // Type specifies which kind of metrics should be associated with instance (postgresql, pgbouncer, etc...)
	ServiceID   string   // Service identifier is unique key across all instances
	ProjectID   string   // Project identifier is unique key across all projects (project may have several instances)
	Pid         int32    // Process identifier
	Host        string   // Hostname used as part of a connection string (related to postgresql, pgbouncer)
	Port        uint16   // Port used as part of a connection string (related to postgresql, pgbouncer)
	User        string   // Username used as part of a connection string (related to postgresql, pgbouncer)
	Password    string   // Password used as part of a connection string (related to postgresql, pgbouncer)
	Dbname      string   // Database name used as part of a connection string (related to postgresql, pgbouncer)
	Exporter    Exporter // Exporter associated with instance
}

// Exporter is an interface for prometheus.Collector
type Exporter interface {
	Describe(chan<- *prometheus.Desc)
	Collect(chan<- prometheus.Metric)
}