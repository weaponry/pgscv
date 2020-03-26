//
package main

import (
	"fmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/alecthomas/kingpin.v2"
	"os"
	"pgscv/app"
	"time"
)

var (
	binName, appName, gitCommit, gitBranch string
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})
	//log.Logger = log.With().Caller().Logger().Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})

	var (
		metricServiceBaseURL = kingpin.Flag("metric-service-url", "Metric service URL push to").Default("").Envar("METRIC_SERVICE_BASE_URL").String()
		metricsSendInterval  = kingpin.Flag("send-interval", "Interval between pushes").Default("60s").Envar("SEND_INTERVAL").Duration()
		doBootstrap          = kingpin.Flag("bootstrap", "Run bootstrap, requires root privileges").Default("false").Envar("BOOTSTRAP").Bool()
		apiKey               = kingpin.Flag("api-key", "Use api key").Default("").Envar("API_KEY").String()
		postgresUsername     = kingpin.Flag("pg-username", "Default username used for connecting to all discovered Postgres services").Default("weaponry_app").Envar("PG_USERNAME").String()
		postgresPassword     = kingpin.Flag("pg-password", "Default password used for connecting to all discovered Postgres services").Default("").Envar("PG_PASSWORD").String()
		pgbouncerUsername    = kingpin.Flag("pgb-username", "Default username used for connecting to all discovered Pgbouncer services").Default("weaponry_app").Envar("PGB_USERNAME").String()
		pgbouncerPassword    = kingpin.Flag("pgb-password", "Default password used for connecting to all discovered Pgbouncer services").Default("").Envar("PGB_PASSWORD").String()
		urlStrings           = kingpin.Flag("url", "Postgres/Pgbouncer service URL, disables auto-discovery, can be used multiple times").Strings()
		showver              = kingpin.Flag("version", "show version and exit").Default().Bool()
		logLevel             = kingpin.Flag("log-level", "set log level: debug, info, warn, error").Default("info").Envar("LOG_LEVEL").String()
	)
	kingpin.Parse()

	var sc = &app.Config{
		Logger:               log.Logger,
		MetricServiceBaseURL: *metricServiceBaseURL,
		MetricsSendInterval:  *metricsSendInterval,
		ProjectIDStr:         app.DecodeProjectIDStr(*apiKey),
		ScheduleEnabled:      false,
		APIKey:               *apiKey,
		BootstrapBinaryName:  binName,
		URLStrings:           *urlStrings,
		Credentials: app.Credentials{
			PostgresUser:  *postgresUsername,
			PostgresPass:  *postgresPassword,
			PgbouncerUser: *pgbouncerUsername,
			PgbouncerPass: *pgbouncerPassword,
		},
	}

	// TODO: add config validations, for: 1) api-key 2) send-interval 3) etc...

	switch *logLevel {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "info":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	if *showver {
		fmt.Printf("%s %s-%s\n", appName, gitCommit, gitBranch)
		os.Exit(0)
	}

	if *doBootstrap {
		os.Exit(app.RunBootstrap(sc))
	}

	// если указан апи-ключ, то из него по-любому должен быть вытащен ид проекта
	if sc.APIKey != "" && sc.ProjectIDStr == "" {
		log.Fatal().Msg("unknown project identifier")
	}

	// use schedulers in push mode
	if sc.MetricServiceBaseURL != "" {
		sc.ScheduleEnabled = true
	}

	if err := app.Start(sc); err != nil {
		log.Error().Err(err).Msg("error occurred:")
	}

	log.Info().Msg("Graceful shutdown")
}