package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// libreTimeout bounds one read. The collection loop waits on this, so it is
// kept well below any sensible sampling interval.
const libreTimeout = 3 * time.Second

// maxLibreBytes caps the response. A machine with every sensor enumerated
// produces a few hundred kilobytes, so this only guards against a wrong URL
// pointing at something unbounded.
const maxLibreBytes = 4 << 20

// libreSource reads the CPU temperature from a LibreHardwareMonitor web server.
//
// Windows exposes no die temperature to user mode: reading the processor's own
// sensor takes a kernel driver, and shipping one would mean signing and
// installing a driver on every node. So where a real reading is wanted, the
// agent borrows it from a tool that already carries that driver, and falls back
// to its own ACPI probe whenever the server is not answering.
type libreSource struct {
	url    string
	client *http.Client
	log    *slog.Logger

	mu      sync.Mutex
	failing bool
}

// newLibreSource prepares the reader; a blank address disables it. The address
// may be given as a bare host and port, and the data endpoint is appended when
// it carries no path of its own.
func newLibreSource(raw string, log *slog.Logger) *libreSource {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		// Refusing to start over this would trade every metric for one of them,
		// so it is reported and the built-in probe stands in.
		log.Error("LibreHardwareMonitorUrl is not a usable address; using the built-in CPU temperature probe",
			"url", raw, "err", err)
		return nil
	}
	if strings.Trim(parsed.Path, "/") == "" {
		parsed.Path = "/data.json"
	}

	return &libreSource{
		url:    parsed.String(),
		client: &http.Client{Timeout: libreTimeout},
		log:    log,
	}
}

// cpuTemperature returns the package reading, or nil when the server cannot be
// reached or reports no processor sensor. A nil receiver is a source that was
// never configured, which keeps the branch out of the caller.
func (s *libreSource) cpuTemperature() *float64 {
	if s == nil {
		return nil
	}

	root, err := s.fetch()
	if err != nil {
		s.report(err)
		return nil
	}

	value, ok := cpuSensor(root)
	if !ok {
		// The server is up but sees no CPU sensor, which is what a missing or
		// blocked kernel driver looks like from here. Worth saying out loud:
		// the fallback reading that follows looks plausible and is not the die.
		s.report(errors.New("no CPU temperature among the reported sensors"))
		return nil
	}

	s.report(nil)
	return tempC(value)
}

func (s *libreSource) fetch() (*libreNode, error) {
	ctx, cancel := context.WithTimeout(context.Background(), libreTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", s.url, resp.Status)
	}

	var root libreNode
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLibreBytes)).Decode(&root); err != nil {
		return nil, fmt.Errorf("decode %s: %w", s.url, err)
	}
	return &root, nil
}

// report logs a change of state rather than every sample, so a server that is
// down for an hour costs one line instead of one line per collection.
func (s *libreSource) report(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case err != nil && !s.failing:
		s.failing = true
		s.log.Warn("LibreHardwareMonitor unusable; using the built-in CPU temperature probe",
			"url", s.url, "err", err)
	case err == nil && s.failing:
		s.failing = false
		s.log.Info("LibreHardwareMonitor is answering again", "url", s.url)
	}
}

// libreNode is one node of the /data.json tree, which nests computer →
// hardware → sensor type → sensor. Every level shares one shape, so the whole
// document decodes into this single type.
type libreNode struct {
	Text       string      `json:"Text"`
	HardwareID string      `json:"HardwareId"`
	SensorID   string      `json:"SensorId"`
	Type       string      `json:"Type"`
	Value      string      `json:"Value"`
	RawValue   libreValue  `json:"RawValue"`
	Children   []libreNode `json:"Children"`
}

// libreValue holds a sensor's raw reading, whose JSON type depends on the
// release: 0.9.6 and earlier serve the same unit-bearing string as Value, while
// later builds serve a bare number. Decoding it as either one alone makes the
// entire document unreadable on the other half of the releases — and a decode
// failure is indistinguishable from a server that is down, so the reading would
// quietly fall back to the built-in probe instead of reporting anything wrong.
type libreValue struct {
	number   float64
	isNumber bool
	text     string
}

// UnmarshalJSON never fails: a field in a shape this does not recognise reads as
// absent, which costs one sensor rather than the whole snapshot.
func (v *libreValue) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &v.number); err == nil {
		v.isNumber = true
		return nil
	}
	json.Unmarshal(data, &v.text)
	return nil
}

// cpuTempRanks order the sensors that can stand for the processor as a whole.
// The package sensor is the one worth reporting; the hottest core is the next
// best thing; an average understates exactly the peaks worth watching, so it
// sits behind both but still ahead of an arbitrary single core.
var cpuTempRanks = map[string]int{
	"cpu package":      0,
	"core (tctl/tdie)": 0,
	"core (tctl)":      0,
	"core (tdie)":      0,
	"core max":         1,
	"cpu cores":        1,
	"core average":     2,
}

// perCoreRank is where an unrecognised CPU temperature sorts: behind every
// named summary sensor, ahead of nothing.
const perCoreRank = 3

// tjMaxSuffix marks the sensors reporting headroom rather than temperature.
// They are typed as temperatures, sit under the processor, and are measured in
// degrees, but a cool chip reports a large distance — so treating one as a
// reading turns idle into the hottest thing on the dashboard.
const tjMaxSuffix = "distance to tjmax"

// cpuSensor walks the tree for the best CPU temperature it carries.
func cpuSensor(root *libreNode) (float64, bool) {
	bestRank, best, found := 0, 0.0, false

	var walk func(node *libreNode, inCPU bool)
	walk = func(node *libreNode, inCPU bool) {
		// Attribution comes from the identifiers rather than from the labels,
		// which are model names and differ per machine.
		inCPU = inCPU || isCPUIdentifier(node.HardwareID)

		label := strings.ToLower(node.Text)

		if (inCPU || isCPUIdentifier(node.SensorID)) && node.isTemperature() && !strings.HasSuffix(label, tjMaxSuffix) {
			if value, ok := node.temperature(); ok {
				rank, named := cpuTempRanks[label]
				if !named {
					rank = perCoreRank
				}
				if !found || rank < bestRank || (rank == bestRank && value > best) {
					bestRank, best, found = rank, value, true
				}
			}
		}

		for i := range node.Children {
			walk(&node.Children[i], inCPU)
		}
	}
	walk(root, false)

	return best, found
}

// isCPUIdentifier reports whether a LibreHardwareMonitor identifier such as
// /intelcpu/0/temperature/0 belongs to a processor.
func isCPUIdentifier(id string) bool {
	return strings.HasPrefix(id, "/intelcpu/") || strings.HasPrefix(id, "/amdcpu/")
}

// isTemperature covers both the current servers, which type their sensors, and
// the older ones that only formatted a unit into the value. Where the type is
// missing, the unit is the only signal — and it has to be recognised in either
// scale, or a machine set to Fahrenheit reads as having no sensors at all.
func (n *libreNode) isTemperature() bool {
	if n.Type != "" {
		return n.Type == "Temperature"
	}
	_, ok := celsius(n.Value)
	return ok
}

// temperature reads the node's value in degrees Celsius.
func (n *libreNode) temperature() (float64, bool) {
	// A numeric raw value is the sensor's own reading, which is always Celsius.
	if n.RawValue.isNumber {
		return n.RawValue.number, true
	}
	// Otherwise both fields carry the formatted string, and its unit has to be
	// read rather than assumed.
	for _, text := range []string{n.RawValue.text, n.Value} {
		if value, ok := celsius(text); ok {
			return value, true
		}
	}
	return 0, false
}

// celsius parses a formatted reading such as "64.5 °C". The unit travels with
// the number because the server formats for display, in whatever unit and with
// whatever decimal separator the serving machine is set to, so Fahrenheit is
// converted rather than read as though it were Celsius.
func celsius(text string) (float64, bool) {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return 0, false
	}

	value, err := strconv.ParseFloat(strings.Replace(fields[0], ",", ".", 1), 64)
	if err != nil {
		return 0, false
	}

	switch strings.TrimPrefix(fields[len(fields)-1], "°") {
	case "C":
		return value, true
	case "F":
		return (value - 32) * 5 / 9, true
	}
	return 0, false
}
