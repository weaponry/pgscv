package collector

import (
	"bufio"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/weaponry/pgscv/internal/filter"
	"github.com/weaponry/pgscv/internal/log"
	"io"
	"os"
	"strconv"
	"strings"
)

type netdevCollector struct {
	bytes   typedDesc
	packets typedDesc
	events  typedDesc
}

// NewNetdevCollector returns a new Collector exposing network interfaces stats.
func NewNetdevCollector(labels prometheus.Labels) (Collector, error) {
	return &netdevCollector{
		bytes: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName("node", "network", "bytes_total"),
				"Total number of bytes processed by network device, by each direction.",
				[]string{"device", "type"}, labels,
			), valueType: prometheus.CounterValue,
		},
		packets: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName("node", "network", "packets_total"),
				"Total number of packets processed by network device, by each direction.",
				[]string{"device", "type"}, labels,
			), valueType: prometheus.CounterValue,
		},
		events: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName("node", "network", "events_total"),
				"Total number of events occurred on network device, by each type and direction.",
				[]string{"device", "type", "event"}, labels,
			), valueType: prometheus.CounterValue,
		},
	}, nil
}

// Update method collects network interfaces statistics
func (c *netdevCollector) Update(config Config, ch chan<- prometheus.Metric) error {
	stats, err := getNetdevStats(config.Filters["netdev/device"])
	if err != nil {
		return fmt.Errorf("get /proc/net/dev stats failed: %s", err)
	}

	for device, stat := range stats {
		if len(stat) < 16 {
			log.Warnf("too few stats columns (%d), skip", len(stat))
			continue
		}

		// recv
		ch <- c.bytes.mustNewConstMetric(stat[0], device, "recv")
		ch <- c.packets.mustNewConstMetric(stat[1], device, "recv")
		ch <- c.events.mustNewConstMetric(stat[2], device, "recv", "errs")
		ch <- c.events.mustNewConstMetric(stat[3], device, "recv", "drop")
		ch <- c.events.mustNewConstMetric(stat[4], device, "recv", "fifo")
		ch <- c.events.mustNewConstMetric(stat[5], device, "recv", "frame")
		ch <- c.events.mustNewConstMetric(stat[6], device, "recv", "compressed")
		ch <- c.events.mustNewConstMetric(stat[7], device, "recv", "multicast")

		// sent
		ch <- c.bytes.mustNewConstMetric(stat[8], device, "sent")
		ch <- c.packets.mustNewConstMetric(stat[9], device, "sent")
		ch <- c.events.mustNewConstMetric(stat[10], device, "sent", "errs")
		ch <- c.events.mustNewConstMetric(stat[11], device, "sent", "drop")
		ch <- c.events.mustNewConstMetric(stat[12], device, "sent", "fifo")
		ch <- c.events.mustNewConstMetric(stat[13], device, "sent", "colls")
		ch <- c.events.mustNewConstMetric(stat[14], device, "sent", "carrier")
		ch <- c.events.mustNewConstMetric(stat[15], device, "sent", "compressed")
	}

	return nil
}

// getNetdevStats is the intermediate function which opens stats file and run stats parser for extracting stats.
func getNetdevStats(filter filter.Filter) (map[string][]float64, error) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	return parseNetdevStats(file, filter)
}

// parseNetdevStats accepts file descriptor, reads file content and produces stats.
func parseNetdevStats(r io.Reader, filter filter.Filter) (map[string][]float64, error) {
	log.Debug("parse network devices stats")

	scanner := bufio.NewScanner(r)

	// Stats file /proc/net/dev has header consisting of two lines. Read the header and check content to make sure this is proper file.
	for i := 0; i < 2; i++ {
		scanner.Scan()
		parts := strings.Split(scanner.Text(), "|")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid input, '%s': wrong number of values", scanner.Text())
		}
	}

	var stats = map[string][]float64{}

	for scanner.Scan() {
		values := strings.Fields(scanner.Text())

		device := strings.TrimRight(values[0], ":")
		if !filter.Pass(device) {
			log.Debugf("ignore device %s", device)
			continue
		}
		log.Debugf("pass device %s", device)

		// Create float64 slice for values, parse line except first three values (major/minor/device)
		stat := make([]float64, len(values)-1)
		for i := range stat {
			value, err := strconv.ParseFloat(values[i+1], 64)
			if err != nil {
				log.Errorf("invalid input, parse '%s' failed: %s, skip", values[i+1], err.Error())
				continue
			}
			stat[i] = value
		}

		stats[device] = stat
	}

	return stats, scanner.Err()
}
