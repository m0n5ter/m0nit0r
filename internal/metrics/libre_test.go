package metrics

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// dataJSON is a trimmed capture of what LibreHardwareMonitor 0.9.6 actually
// served on an i9-13900H: one tree of identically shaped nodes nesting computer
// → hardware → sensor type → sensor.
//
// Three details here are load-bearing and were all taken from the real
// response rather than from the source. RawValue is a unit-bearing string, not
// a number — later releases changed it, and decoding only one shape makes the
// whole document unreadable. The degree sign arrives as a ° escape. And
// the processor reports "Distance to TjMax" sensors, which are typed as
// temperatures but measure headroom, so a cool chip reports a large number.
//
// The GPU and the drive are here because they report temperatures too, and
// picking one of them instead of the processor is the obvious way to get this
// wrong.
const dataJSON = `{
  "id": 0, "Text": "Sensor", "Min": "", "Value": "", "Max": "",
  "Children": [{
    "id": 1, "Text": "MONSTER-PC", "Min": "", "Value": "", "Max": "",
    "Children": [
      {
        "id": 2, "Text": "13th Gen Intel Core i9-13900H", "HardwareId": "/intelcpu/0",
        "Min": "", "Value": "", "Max": "",
        "Children": [{
          "id": 3, "Text": "Temperatures", "Min": "", "Value": "", "Max": "",
          "Children": [
            {"id": 4, "Text": "Core Max", "SensorId": "/intelcpu/0/temperature/0", "Type": "Temperature",
             "Min": "66.0 °C", "Value": "72.0 °C", "Max": "94.0 °C", "RawValue": "72.0 °C"},
            {"id": 5, "Text": "Core Average", "SensorId": "/intelcpu/0/temperature/1", "Type": "Temperature",
             "Min": "60.0 °C", "Value": "67.1 °C", "Max": "91.0 °C", "RawValue": "67.1 °C"},
            {"id": 6, "Text": "P-Core #2", "SensorId": "/intelcpu/0/temperature/3", "Type": "Temperature",
             "Min": "40.0 °C", "Value": "72.0 °C", "Max": "91.0 °C", "RawValue": "72.0 °C"},
            {"id": 7, "Text": "CPU Package", "SensorId": "/intelcpu/0/temperature/16", "Type": "Temperature",
             "Min": "39.0 °C", "Value": "64.5 °C", "Max": "95.0 °C", "RawValue": "64.5 °C"},
            {"id": 8, "Text": "P-Core #1 Distance to TjMax", "SensorId": "/intelcpu/0/temperature/17",
             "Type": "Temperature", "Min": "6.0 °C", "Value": "39.0 °C", "Max": "60.0 °C",
             "RawValue": "39.0 °C"}
          ]
        }, {
          "id": 9, "Text": "Powers", "Min": "", "Value": "", "Max": "",
          "Children": [
            {"id": 10, "Text": "CPU Package", "SensorId": "/intelcpu/0/power/0", "Type": "Power",
             "Min": "3.0 W", "Value": "19.3 W", "Max": "94.0 W", "RawValue": "19.3 W"}
          ]
        }]
      },
      {
        "id": 11, "Text": "NVIDIA GeForce RTX 4070", "HardwareId": "/gpu-nvidia/0",
        "Min": "", "Value": "", "Max": "",
        "Children": [{
          "id": 12, "Text": "Temperatures", "Min": "", "Value": "", "Max": "",
          "Children": [
            {"id": 13, "Text": "GPU Core", "SensorId": "/gpu-nvidia/0/temperature/0", "Type": "Temperature",
             "Min": "35.0 °C", "Value": "81.0 °C", "Max": "83.0 °C", "RawValue": "81.0 °C"}
          ]
        }]
      },
      {
        "id": 14, "Text": "Samsung SSD 970 EVO Plus 2TB", "HardwareId": "/nvme/0",
        "Min": "", "Value": "", "Max": "",
        "Children": [{
          "id": 15, "Text": "Temperatures", "Min": "", "Value": "", "Max": "",
          "Children": [
            {"id": 16, "Text": "Composite Temperature", "SensorId": "/nvme/0/temperature/0",
             "Type": "Temperature", "Min": "44.0 °C", "Value": "77.0 °C", "Max": "78.0 °C",
             "RawValue": "77.0 °C"}
          ]
        }]
      }
    ]
  }]
}`

func testSource(t *testing.T, handler http.HandlerFunc) *libreSource {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	source := newLibreSource(server.URL, slog.New(slog.DiscardHandler))
	if source == nil {
		t.Fatal("newLibreSource returned nil for a valid address")
	}
	return source
}

func serveJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/data.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}
}

func TestLibreSourcePrefersThePackageSensor(t *testing.T) {
	source := testSource(t, serveJSON(dataJSON))

	got := source.cpuTemperature()
	if got == nil {
		t.Fatal("cpuTemperature() = nil, want the CPU package reading")
	}
	// Not 72 (the hottest core), not 81 (the GPU), not 77 (the drive).
	if *got != 64.5 {
		t.Errorf("cpuTemperature() = %v, want 64.5", *got)
	}
}

// Later releases serve RawValue as a bare number instead of a string. Both
// shapes have to decode, because getting it wrong fails the whole document and
// looks exactly like a server that is down.
func TestLibreSourceAcceptsNumericRawValues(t *testing.T) {
	const numeric = `{"id":0,"Text":"Sensor","Children":[{"id":1,"Text":"host","HardwareId":"/intelcpu/0",
      "Children":[
        {"id":2,"Text":"CPU Package","SensorId":"/intelcpu/0/temperature/16","Type":"Temperature","Value":"64.5 °C","RawValue":64.5},
        {"id":3,"Text":"P-Core #1","SensorId":"/intelcpu/0/temperature/2","Type":"Temperature","Value":"72.0 °C","RawValue":72.0}
      ]}]}`

	source := testSource(t, serveJSON(numeric))

	got := source.cpuTemperature()
	if got == nil {
		t.Fatal("cpuTemperature() = nil, want the package reading")
	}
	if *got != 64.5 {
		t.Errorf("cpuTemperature() = %v, want 64.5", *got)
	}
}

func TestLibreSourceFallsBackToTheHottestCore(t *testing.T) {
	// A processor whose driver exposes per-core sensors only.
	const noPackage = `{"id":0,"Text":"Sensor","Children":[{"id":1,"Text":"host","HardwareId":"/amdcpu/0",
      "Children":[
        {"id":2,"Text":"CPU Core #1","SensorId":"/amdcpu/0/temperature/0","Type":"Temperature","Value":"55.0 °C","RawValue":"55.0 °C"},
        {"id":3,"Text":"CPU Core #2","SensorId":"/amdcpu/0/temperature/1","Type":"Temperature","Value":"58.5 °C","RawValue":"58.5 °C"}
      ]}]}`

	source := testSource(t, serveJSON(noPackage))

	got := source.cpuTemperature()
	if got == nil {
		t.Fatal("cpuTemperature() = nil, want the hottest core")
	}
	if *got != 58.5 {
		t.Errorf("cpuTemperature() = %v, want 58.5", *got)
	}
}

// A headroom sensor outranks every real reading on an idle chip, so falling
// back to the hottest core must not sweep one up.
func TestLibreSourceIgnoresDistanceToTjMax(t *testing.T) {
	const idle = `{"id":0,"Text":"Sensor","Children":[{"id":1,"Text":"host","HardwareId":"/intelcpu/0",
      "Children":[
        {"id":2,"Text":"P-Core #1","SensorId":"/intelcpu/0/temperature/2","Type":"Temperature","Value":"35.0 °C","RawValue":"35.0 °C"},
        {"id":3,"Text":"P-Core #1 Distance to TjMax","SensorId":"/intelcpu/0/temperature/17","Type":"Temperature","Value":"65.0 °C","RawValue":"65.0 °C"}
      ]}]}`

	source := testSource(t, serveJSON(idle))

	got := source.cpuTemperature()
	if got == nil {
		t.Fatal("cpuTemperature() = nil, want the core reading")
	}
	if *got != 35 {
		t.Errorf("cpuTemperature() = %v, want 35 (65 is the headroom sensor)", *got)
	}
}

// The formatted value carries the locale's decimal separator, since the server
// formats it on the machine it runs on.
func TestLibreSourceParsesLocaleFormattedValues(t *testing.T) {
	const commaDecimals = `{"id":0,"Text":"Sensor","Children":[{"id":1,"Text":"host","HardwareId":"/intelcpu/0",
      "Children":[{"id":2,"Text":"CPU Package","SensorId":"/intelcpu/0/temperature/16","Value":"64,5 °C"}]}]}`

	source := testSource(t, serveJSON(commaDecimals))

	got := source.cpuTemperature()
	if got == nil {
		t.Fatal("cpuTemperature() = nil, want 64.5 parsed from a comma decimal separator")
	}
	if *got != 64.5 {
		t.Errorf("cpuTemperature() = %v, want 64.5", *got)
	}
}

// The formatted value follows the display unit, so a server set to Fahrenheit
// must be converted rather than read as though the number were Celsius.
func TestLibreSourceConvertsFahrenheit(t *testing.T) {
	const fahrenheit = `{"id":0,"Text":"Sensor","Children":[{"id":1,"Text":"host","HardwareId":"/intelcpu/0",
      "Children":[{"id":2,"Text":"CPU Package","SensorId":"/intelcpu/0/temperature/16","Value":"148.1 °F"}]}]}`

	source := testSource(t, serveJSON(fahrenheit))

	got := source.cpuTemperature()
	if got == nil {
		t.Fatal("cpuTemperature() = nil, want the Fahrenheit reading converted")
	}
	if *got != 64.5 {
		t.Errorf("cpuTemperature() = %v, want 64.5", *got)
	}
}

// Sensors are named per hardware, not per kind: the processor reports both a
// power and a temperature called "CPU Package".
func TestLibreSourceIgnoresNonTemperatureSensors(t *testing.T) {
	source := testSource(t, serveJSON(dataJSON))

	got := source.cpuTemperature()
	if got == nil {
		t.Fatal("cpuTemperature() = nil")
	}
	if *got == 19.3 {
		t.Fatal("cpuTemperature() returned the package power reading")
	}
}

func TestLibreSourceWithoutACPUSensor(t *testing.T) {
	// Everything but the processor, which is what a blocked or missing kernel
	// driver looks like over this interface.
	const gpuOnly = `{"id":0,"Text":"Sensor","Children":[{"id":1,"Text":"host","HardwareId":"/gpu-nvidia/0",
      "Children":[{"id":2,"Text":"GPU Core","SensorId":"/gpu-nvidia/0/temperature/0","Type":"Temperature","RawValue":81.0}]}]}`

	source := testSource(t, serveJSON(gpuOnly))

	if got := source.cpuTemperature(); got != nil {
		t.Errorf("cpuTemperature() = %v, want nil", *got)
	}
}

func TestLibreSourceWhenTheServerIsDown(t *testing.T) {
	source := testSource(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	})

	if got := source.cpuTemperature(); got != nil {
		t.Errorf("cpuTemperature() = %v, want nil", *got)
	}
}

func TestNewLibreSource(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	cases := []struct {
		name string
		in   string
		want string // empty means the source should be disabled
	}{
		{"bare host and port", "127.0.0.1:8085", "http://127.0.0.1:8085/data.json"},
		{"scheme and host", "http://127.0.0.1:8085", "http://127.0.0.1:8085/data.json"},
		{"trailing slash", "http://127.0.0.1:8085/", "http://127.0.0.1:8085/data.json"},
		{"explicit path is kept", "http://127.0.0.1:8085/other.json", "http://127.0.0.1:8085/other.json"},
		{"credentials are kept", "http://user:pass@127.0.0.1:8085", "http://user:pass@127.0.0.1:8085/data.json"},
		{"unset", "", ""},
		{"blank", "   ", ""},
		{"no host", "http://", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := newLibreSource(tc.in, log)
			switch {
			case tc.want == "":
				if source != nil {
					t.Errorf("newLibreSource(%q) = %q, want disabled", tc.in, source.url)
				}
			case source == nil:
				t.Errorf("newLibreSource(%q) = disabled, want %q", tc.in, tc.want)
			case source.url != tc.want:
				t.Errorf("newLibreSource(%q) = %q, want %q", tc.in, source.url, tc.want)
			}
		})
	}
}

// A source that was never configured is asked for a reading on every sample, so
// the nil case has to be safe rather than merely unreached.
func TestNilLibreSourceIsSafe(t *testing.T) {
	var source *libreSource
	if got := source.cpuTemperature(); got != nil {
		t.Errorf("cpuTemperature() on a nil source = %v, want nil", *got)
	}
}
