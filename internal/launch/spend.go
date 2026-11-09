package launch

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

// Vertex launches count their spend the way the desktop app does: Claude Code
// exports a claude_code.api_request log event with its own cost for every
// request (OTLP/HTTP JSON) to a loopback receiver at an unguessable path, and
// the costs are appended to the profile's account in the credit ledger.
func vertexLaunch(p store.Profile, executable string, args []string, env map[string]string, errOut io.Writer) error {
	receiver, err := startSpendReceiver()
	if err != nil {
		fmt.Fprintf(errOut, "claudock: %s: could not start the local usage receiver; this run will not update the budget estimate.\n", p.Name)
		return supervisedLaunch(executable, args, environmentList(env))
	}
	telemetryEnvironment(env, receiver.endpoint)
	runErr := supervisedLaunch(executable, args, environmentList(env))
	if _, err := store.RecordCredit(p, receiver.stop()); err != nil {
		fmt.Fprintf(errOut, "claudock: %s: could not save this run's spend to the budget estimate: %v\n", p.Name, err)
	}
	return runErr
}

// telemetryEnvironment replaces every inherited OTEL_* variable and
// CLAUDE_CODE_ENABLE_TELEMETRY with log export to endpoint. Metrics and traces
// get no exporter.
func telemetryEnvironment(env map[string]string, endpoint string) {
	for name := range env {
		if strings.HasPrefix(name, "OTEL_") || name == "CLAUDE_CODE_ENABLE_TELEMETRY" {
			delete(env, name)
		}
	}
	env["CLAUDE_CODE_ENABLE_TELEMETRY"] = "1"
	env["OTEL_LOGS_EXPORTER"] = "otlp"
	env["OTEL_EXPORTER_OTLP_PROTOCOL"] = "http/json"
	env["OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"] = "http/json"
	env["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] = endpoint
	env["OTEL_LOGS_EXPORT_INTERVAL"] = "1000"
}

type spendReceiver struct {
	server   *http.Server
	endpoint string
	mu       sync.Mutex
	requests []store.CreditRequest
}

func startSpendReceiver() (*spendReceiver, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	path := "/" + hex.EncodeToString(token) + "/v1/logs"
	r := &spendReceiver{endpoint: "http://" + listener.Addr().String() + path}
	r.server = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != path || req.Header.Get("Origin") != "" {
			http.NotFound(w, req)
			return
		}
		if media, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type")); media != "application/json" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 16*1024*1024))
		if err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		if encoding := req.Header.Get("Content-Encoding"); encoding == "" || strings.EqualFold(encoding, "identity") {
			found := creditRequests(body)
			r.mu.Lock()
			r.requests = append(r.requests, found...)
			r.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})}
	go func() { _ = r.server.Serve(listener) }()
	return r, nil
}

// stop waits briefly for exports in flight and returns every request received.
func (r *spendReceiver) stop() []store.CreditRequest {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.server.Shutdown(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.CreditRequest(nil), r.requests...)
}

// creditRequests reads the api_request events of an OTLP/HTTP JSON log export.
// An event's ID is the SHA-256 of its session.id, event.sequence,
// event.timestamp (or timeUnixNano), request_id, and client_request_id joined
// with U+001F, absent fields empty, so a batch sent again adds nothing. Other
// events, and api_request events without a session, a time, or a valid
// non-negative cost, are ignored.
func creditRequests(body []byte) []store.CreditRequest {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if decoder.Decode(&root) != nil {
		return nil
	}
	var result []store.CreditRequest
	for _, resource := range otlpObjects(root["resourceLogs"]) {
		for _, scope := range otlpObjects(resource["scopeLogs"]) {
			for _, record := range otlpObjects(scope["logRecords"]) {
				if request, ok := creditRequest(record); ok {
					result = append(result, request)
				}
			}
		}
	}
	return result
}

func creditRequest(record map[string]any) (store.CreditRequest, bool) {
	list, ok := record["attributes"].([]any)
	if !ok {
		return store.CreditRequest{}, false
	}
	attributes := make(map[string]map[string]any)
	for _, item := range otlpObjects(list) {
		key, keyOK := item["key"].(string)
		value, valueOK := item["value"].(map[string]any)
		if keyOK && valueOK {
			attributes[key] = value
		}
	}
	body, _ := record["body"].(map[string]any)
	bodyText, _ := body["stringValue"].(string)
	name, _ := otlpString(attributes["event.name"])
	if name != "api_request" && bodyText != "claude_code.api_request" {
		return store.CreditRequest{}, false
	}
	session, _ := otlpString(attributes["session.id"])
	cost, costOK := otlpCost(attributes)
	if session == "" || !costOK {
		return store.CreditRequest{}, false
	}
	timestamp, hasTimestamp := otlpString(attributes["event.timestamp"])
	nanoseconds, hasNanoseconds := unixNanoseconds(record["timeUnixNano"])
	if !hasNanoseconds {
		nanoseconds, hasNanoseconds = unixNanoseconds(record["observedTimeUnixNano"])
	}
	var at time.Time
	parsed := false
	if hasTimestamp {
		at, parsed = otlpTime(timestamp)
	}
	if !parsed && hasNanoseconds {
		at, parsed = time.Unix(0, nanoseconds).UTC(), true
	}
	if !parsed {
		return store.CreditRequest{}, false
	}
	sequence := ""
	if value, ok := otlpInteger(attributes["event.sequence"]); ok {
		sequence = strconv.FormatInt(value, 10)
	}
	moment := timestamp
	if !hasTimestamp && hasNanoseconds {
		moment = strconv.FormatInt(nanoseconds, 10)
	}
	requestID, _ := otlpString(attributes["request_id"])
	clientRequestID, _ := otlpString(attributes["client_request_id"])
	sum := sha256.Sum256([]byte(strings.Join([]string{session, sequence, moment, requestID, clientRequestID}, "\x1f")))
	request := store.CreditRequest{ID: hex.EncodeToString(sum[:]), SessionID: session, At: at, CostUSD: cost}
	if model, ok := otlpString(attributes["model"]); ok {
		request.Model = &model
	}
	return request, true
}

// otlpObjects is a JSON array of objects, or nil when any element is not one.
func otlpObjects(raw any) []map[string]any {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(list))
	for _, item := range list {
		object, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		result = append(result, object)
	}
	return result
}

func otlpString(value map[string]any) (string, bool) {
	text, ok := value["stringValue"].(string)
	return text, ok
}

// otlpInteger reads intValue, which OTLP JSON writes as a number or a decimal string.
func otlpInteger(value map[string]any) (int64, bool) {
	switch raw := value["intValue"].(type) {
	case string:
		parsed, err := strconv.ParseInt(raw, 10, 64)
		return parsed, err == nil
	case json.Number:
		number, err := raw.Float64()
		if err != nil || math.Trunc(number) != number || math.Abs(number) >= 9e15 {
			return 0, false
		}
		return int64(number), true
	}
	return 0, false
}

func otlpNumber(value map[string]any) (float64, bool) {
	if raw, present := value["doubleValue"]; present {
		number, ok := raw.(json.Number)
		if !ok {
			return 0, false
		}
		parsed, err := number.Float64()
		return parsed, err == nil
	}
	integer, ok := otlpInteger(value)
	return float64(integer), ok
}

// otlpCost is cost_usd when present, which must then be valid; otherwise cost_usd_micros.
func otlpCost(attributes map[string]map[string]any) (float64, bool) {
	var value float64
	var ok bool
	if cost, present := attributes["cost_usd"]; present {
		value, ok = otlpNumber(cost)
	} else {
		var micros int64
		micros, ok = otlpInteger(attributes["cost_usd_micros"])
		value = float64(micros) / 1_000_000
	}
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= 1_000_000 {
		return 0, false
	}
	return value, true
}

func unixNanoseconds(raw any) (int64, bool) {
	var value int64
	switch raw := raw.(type) {
	case string:
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, false
		}
		value = parsed
	case json.Number:
		if parsed, err := raw.Int64(); err == nil {
			value = parsed
		} else if number, err := raw.Float64(); err == nil && number < 9.2e18 && number > -9.2e18 {
			value = int64(number)
		} else {
			return 0, false
		}
	default:
		return 0, false
	}
	return value, value > 0
}

func otlpTime(text string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, text)
	return at.UTC(), err == nil
}
