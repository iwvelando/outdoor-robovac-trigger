package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	influx "github.com/influxdata/influxdb-client-go/v2"
	influxAPI "github.com/influxdata/influxdb-client-go/v2/api"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"net/http"
	"os"
)

// BuildVersion is the software build version
var BuildVersion = "UNKNOWN"

// Configuration represents a YAML-formatted config file
type Configuration struct {
	Vacuum   Vacuum
	Query    Query
	InfluxDB InfluxDB
}

// Vacuum holds the parameters for controlling the robot vacuum
type Vacuum struct {
	WebhookStart  string
	WebhookStop   string
	SkipVerifySsl bool
}

// Query holds the parameters for querying the forecast query
type Query struct {
	LookbackDuration    string
	LookforwardDuration string
}

// InfluxDB holds the connection parameters for InfluxDB
type InfluxDB struct {
	Address         string
	Username        string
	Password        string
	Measurement     string
	Field           string
	Database        string
	RetentionPolicy string
	Token           string
	Organization    string
	Bucket          string
	SkipVerifySsl   bool
}

// CliInputs holds the data passed in via CLI parameters
type CliInputs struct {
	BuildVersion string
	Config       string
	Action       string
	ShowVersion  bool
}

// LoadConfiguration takes a file path as input and loads the YAML-formatted
// configuration there.
func LoadConfiguration(configPath string) (*Configuration, error) {
	viper.SetConfigFile(configPath)
	viper.AutomaticEnv()

	viper.SetConfigType("yml")

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file, %s", err)
	}

	var configuration Configuration
	err := viper.Unmarshal(&configuration)
	if err != nil {
		return nil, fmt.Errorf("unable to decode into struct, %s", err)
	}

	return &configuration, nil
}

// Connect establishes an InfluxDB client
func InfluxConnect(config *Configuration) (influx.Client, influxAPI.QueryAPI, error) {
	var auth string
	if config.InfluxDB.Token != "" {
		auth = config.InfluxDB.Token
	} else if config.InfluxDB.Username != "" && config.InfluxDB.Password != "" {
		auth = fmt.Sprintf("%s:%s", config.InfluxDB.Username, config.InfluxDB.Password)
	} else {
		auth = ""
	}

	options := influx.DefaultOptions().
		SetTLSConfig(&tls.Config{
			InsecureSkipVerify: config.InfluxDB.SkipVerifySsl,
		})
	client := influx.NewClientWithOptions(config.InfluxDB.Address, auth, options)

	queryAPI := client.QueryAPI(config.InfluxDB.Organization)

	return client, queryAPI, nil
}

func main() {

	cliInputs := CliInputs{
		BuildVersion: BuildVersion,
	}
	flags := flag.NewFlagSet("outdoor-robovac-trigger", 0)
	flags.StringVar(&cliInputs.Config, "config", "config.yaml", "Set the location for the YAML config file")
	flags.StringVar(&cliInputs.Action, "action", "start", "Set action for outdoor-robovac-trigger; start will decide whether to start the vacuum and stop will decide whether to stop it based on the forecast")
	flags.BoolVar(&cliInputs.ShowVersion, "version", false, "Print the version of outdoor-robovac-trigger")
	flags.Parse(os.Args[1:])

	if cliInputs.ShowVersion {
		fmt.Println(cliInputs.BuildVersion)
		os.Exit(0)
	}

	if cliInputs.Action != "start" && cliInputs.Action != "stop" {
		log.WithFields(log.Fields{
			"op": "main",
		}).Fatal("CLI parameter action must be either start or stop")
	}

	configuration, err := LoadConfiguration(cliInputs.Config)
	if err != nil {
		log.WithFields(log.Fields{
			"op":    "LoadConfiguration",
			"error": err,
		}).Fatal("failed to parse configuration")
	}

	influxClient, queryAPI, err := InfluxConnect(configuration)
	if err != nil {
		log.WithFields(log.Fields{
			"op":    "influxdb.Connect",
			"error": err,
		}).Fatal("failed to authenticate to InfluxDB")
	}
	defer influxClient.Close()

	var bucket string
	if configuration.InfluxDB.Bucket != "" {
		bucket = configuration.InfluxDB.Bucket
	} else if configuration.InfluxDB.Database != "" && configuration.InfluxDB.RetentionPolicy != "" {
		bucket = fmt.Sprintf("%s/%s", configuration.InfluxDB.Database, configuration.InfluxDB.RetentionPolicy)
	} else {
		log.WithFields(log.Fields{
			"op":    "main",
			"error": err,
		}).Fatal("must configure at least one of bucket or database/retention policy")
	}

	var pastPrecip float64
	var futurePrecip float64
	if cliInputs.Action == "start" {
		// Query past precipitation
		query := fmt.Sprintf(`from(bucket: "%s")
			|> range(start: -%s)
			|> filter(fn: (r) => r["_measurement"] == "%s" and r["_field"] == "%s")
			|> max(column: "_value")`,
			bucket, configuration.Query.LookbackDuration,
			configuration.InfluxDB.Measurement, configuration.InfluxDB.Field)

		result, err := queryAPI.Query(context.Background(), query)

		if err != nil {
			log.WithFields(log.Fields{
				"op":    "main",
				"error": err,
			}).Fatal("failed to query lookback data from InfluxDB")
		}

		result.Next()
		if result.Err() != nil {
			log.WithFields(log.Fields{
				"op":    "main",
				"error": err,
			}).Fatal("failed parsing lookback data from InfluxDB")
		}
		pastPrecip = result.Record().Value().(float64)
		result.Close()
	}

	// Query future data
	query := fmt.Sprintf(`import "experimental"
		from(bucket: "%s")
			|> range(start: now(), stop: experimental.addDuration(d: %s, to: now()))
			|> filter(fn: (r) => r["_measurement"] == "%s" and r["_field"] == "%s")
			|> max(column: "_value")`,
		bucket, configuration.Query.LookforwardDuration,
		configuration.InfluxDB.Measurement, configuration.InfluxDB.Field)

	result, err := queryAPI.Query(context.Background(), query)

	if err != nil {
		log.WithFields(log.Fields{
			"op":    "main",
			"error": err,
		}).Fatal("failed to query lookforward data from InfluxDB")
	}

	result.Next()
	if result.Err() != nil {
		log.WithFields(log.Fields{
			"op":    "main",
			"error": err,
		}).Fatal("failed parsing lookforward data from InfluxDB")
	}
	futurePrecip = result.Record().Value().(float64)
	result.Close()

	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: configuration.Vacuum.SkipVerifySsl}

	// Conditionally launch robot vacuum
	if cliInputs.Action == "start" {
		if pastPrecip == 0.0 && futurePrecip == 0.0 {
			_, err := http.Get(configuration.Vacuum.WebhookStart)
			if err != nil {
				log.WithFields(log.Fields{
					"op":    "main",
					"error": err,
				}).Fatal("failed to start robot vacuum")
			} else {
				log.WithFields(log.Fields{
					"op":                  "main",
					"lookbackDuration":    configuration.Query.LookbackDuration,
					"lookforwardDuration": configuration.Query.LookforwardDuration,
				}).Info("started robot vacuum based on no precipitation in forecast")
			}
		} else if pastPrecip > 0.0 && futurePrecip > 0.0 {
			log.WithFields(log.Fields{
				"op":                  "main",
				"lookbackDuration":    configuration.Query.LookbackDuration,
				"lookforwardDuration": configuration.Query.LookforwardDuration,
			}).Info("precipitation found both in past and future forecast, not starting vacuum")
		} else if pastPrecip > 0.0 {
			log.WithFields(log.Fields{
				"op":                  "main",
				"lookbackDuration":    configuration.Query.LookbackDuration,
				"lookforwardDuration": configuration.Query.LookforwardDuration,
			}).Info("precipitation found in past weather, not starting vacuum")
		} else if futurePrecip > 0.0 {
			log.WithFields(log.Fields{
				"op":                  "main",
				"lookbackDuration":    configuration.Query.LookbackDuration,
				"lookforwardDuration": configuration.Query.LookforwardDuration,
			}).Info("precipitation found in future forecast, not starting vacuum")
		}
	}

	// Conditionally stop robot vacuum
	if cliInputs.Action == "stop" {
		if futurePrecip > 0.0 {
			_, err := http.Get(configuration.Vacuum.WebhookStop)
			if err != nil {
				log.WithFields(log.Fields{
					"op":    "main",
					"error": err,
				}).Fatal("failed to stop robot vacuum")
			} else {
				log.WithFields(log.Fields{
					"op":                  "main",
					"lookforwardDuration": configuration.Query.LookforwardDuration,
				}).Info("stopped robot vacuum based on precipitation in forecast")
			}
		} else {
			log.WithFields(log.Fields{
				"op":                  "main",
				"lookforwardDuration": configuration.Query.LookforwardDuration,
			}).Info("forecast is dry, not stopping vacuum")
		}
	}

	os.Exit(0)

}
