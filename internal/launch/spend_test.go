package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

const otlpExport = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[
{"timeUnixNano":"1794236400000000000","body":{"stringValue":"claude_code.api_request"},"attributes":[
 {"key":"event.name","value":{"stringValue":"api_request"}},{"key":"session.id","value":{"stringValue":"s1"}},
 {"key":"event.sequence","value":{"intValue":"3"}},{"key":"event.timestamp","value":{"stringValue":"2026-11-09T15:00:00.123Z"}},
 {"key":"request_id","value":{"stringValue":"req_1"}},{"key":"model","value":{"stringValue":"claude-test"}},
 {"key":"cost_usd","value":{"doubleValue":0.173218}}]},
{"timeUnixNano":"1794236401000000000","attributes":[
 {"key":"event.name","value":{"stringValue":"api_request"}},{"key":"session.id","value":{"stringValue":"s1"}},
 {"key":"cost_usd_micros","value":{"intValue":2500}}]},
{"timeUnixNano":"1794236402000000000","attributes":[
 {"key":"event.name","value":{"stringValue":"user_prompt"}},{"key":"session.id","value":{"stringValue":"s1"}},
 {"key":"cost_usd","value":{"doubleValue":9}}]},
{"timeUnixNano":"1794236403000000000","attributes":[
 {"key":"event.name","value":{"stringValue":"api_request"}},{"key":"session.id","value":{"stringValue":"s1"}},
 {"key":"cost_usd","value":{"doubleValue":-1}}]},
{"attributes":[
 {"key":"event.name","value":{"stringValue":"api_request"}},{"key":"session.id","value":{"stringValue":"s1"}},
 {"key":"cost_usd","value":{"doubleValue":1}}]},
{"timeUnixNano":"1794236404000000000","attributes":[
 {"key":"event.name","value":{"stringValue":"api_request"}},
 {"key":"cost_usd","value":{"doubleValue":1}}]}
]}]}]}`

func TestCreditRequestsFromOTLPExport(t *testing.T) {
	requests := creditRequests([]byte(otlpExport))
	if len(requests) != 2 {
		t.Fatalf("requests = %+v", requests)
	}
	first := requests[0]
	id := sha256.Sum256([]byte("s1\x1f3\x1f2026-11-09T15:00:00.123Z\x1freq_1\x1f"))
	if first.ID != hex.EncodeToString(id[:]) || first.SessionID != "s1" || first.CostUSD != 0.173218 || first.Model == nil || *first.Model != "claude-test" || !first.At.Equal(time.Date(2026, 11, 9, 15, 0, 0, 123e6, time.UTC)) {
		t.Fatalf("first request = %+v", first)
	}
	second := requests[1]
	id = sha256.Sum256([]byte("s1\x1f\x1f1794236401000000000\x1f\x1f"))
	if second.ID != hex.EncodeToString(id[:]) || second.CostUSD != 0.0025 || second.Model != nil || !second.At.Equal(time.Unix(0, 1794236401000000000)) {
		t.Fatalf("second request = %+v", second)
	}
	if creditRequests([]byte(`{"resourceLogs":[1]}`)) != nil || creditRequests([]byte(`not json`)) != nil {
		t.Fatal("accepted a malformed export")
	}
}

func TestSpendReceiverAcceptsOnlyItsPath(t *testing.T) {
	r, err := startSpendReceiver()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.endpoint, "http://127.0.0.1:") || !strings.HasSuffix(r.endpoint, "/v1/logs") {
		t.Fatalf("endpoint %q", r.endpoint)
	}
	post := func(url, contentType string) int {
		response, err := http.Post(url, contentType, strings.NewReader(otlpExport))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	base := r.endpoint[:strings.LastIndex(r.endpoint[:len(r.endpoint)-len("/v1/logs")], "/")]
	if status := post(base+"/wrong/v1/logs", "application/json"); status != http.StatusNotFound {
		t.Fatalf("wrong path status %d", status)
	}
	if status := post(r.endpoint, "text/plain"); status != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong type status %d", status)
	}
	if status := post(r.endpoint, "application/json"); status != http.StatusOK {
		t.Fatalf("export status %d", status)
	}
	if requests := r.stop(); len(requests) != 2 || requests[0].CostUSD != 0.173218 {
		t.Fatalf("received %+v", requests)
	}
}

func TestTelemetryEnvOverride(t *testing.T) {
	env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.invalid", "OTEL_METRICS_EXPORTER": "otlp", "CLAUDE_CODE_ENABLE_TELEMETRY": "0", "PATH": "/usr/bin"}
	telemetryEnvironment(env, "http://127.0.0.1:1/token/v1/logs")
	want := map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "1", "OTEL_LOGS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/json", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:1/token/v1/logs", "OTEL_LOGS_EXPORT_INTERVAL": "1000", "PATH": "/usr/bin"}
	if len(env) != len(want) {
		t.Fatalf("environment = %v", env)
	}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("%s = %q, want %q", key, env[key], value)
		}
	}
}
