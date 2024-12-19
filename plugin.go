package traefik_datadog_tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Config defines the plugin configuration
type Config struct {
	GlobalTags             map[string]string `json:"globalTags,omitempty"`             // Global tags for all spans
	PluginName             string            `json:"pluginName,omitempty"`             // Name used for service and operations
	DatadogTracingAgentUrl string            `json:"datadogTracingAgentUrl,omitempty"` // Datadog agent URL
}

// CreateConfig initializes the default plugin configuration
func CreateConfig() *Config {
	return &Config{
		GlobalTags:             map[string]string{},
		DatadogTracingAgentUrl: "http://localhost:8126",
		PluginName:             "tracingplugin",
	}
}

// TracingPlugin defines the plugin structure
type TracingPlugin struct {
	next                   http.Handler
	name                   string
	globalTags             map[string]string
	datadogTracingAgentUrl string
	pluginName             string
}

// New creates a new plugin instance
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	log.Printf("Initializing plugin: name=%s, globalTags=%v, datadogTracingAgentUrl=%s", name, config.GlobalTags, config.DatadogTracingAgentUrl)

	return &TracingPlugin{
		next:                   next,
		name:                   name,
		globalTags:             config.GlobalTags,
		datadogTracingAgentUrl: config.DatadogTracingAgentUrl,
		pluginName:             config.PluginName,
	}, nil
}

// TimingContext wraps a parent context and adds timing functionality
type TimingContext struct {
	parent    context.Context
	startTime time.Time
}

// NewTimingContext creates a new TimingContext
func NewTimingContext(ctx context.Context) *TimingContext {
	return &TimingContext{
		parent:    ctx,
		startTime: time.Now(),
	}
}

// StartTime returns the time when the context was created
func (tc *TimingContext) StartTime() time.Time {
	return tc.startTime
}

// Duration returns the duration since the TimingContext was created
func (tc *TimingContext) Duration() time.Duration {
	return time.Since(tc.startTime)
}

// Deadline delegates to the parent context
func (tc *TimingContext) Deadline() (deadline time.Time, ok bool) {
	return tc.parent.Deadline()
}

// Done delegates to the parent context
func (tc *TimingContext) Done() <-chan struct{} {
	return tc.parent.Done()
}

// Err delegates to the parent context
func (tc *TimingContext) Err() error {
	return tc.parent.Err()
}

// Value delegates to the parent context
func (tc *TimingContext) Value(key interface{}) interface{} {
	return tc.parent.Value(key)
}

// ServeHTTP handles HTTP requests and creates spans
func (p *TracingPlugin) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Wrap the context for timing
	timingCtx := NewTimingContext(req.Context())
	req = req.WithContext(timingCtx)

	// Create a new trace and span
	traceID, spanID := createTraceAndSpan()

	// Inject distributed tracing headers into the request
	injectTraceHeaders(req, traceID, spanID)

	// Pass the request to the next handler
	p.next.ServeHTTP(rw, req)

	// Calculate the duration after the middleware has completed
	if tc, ok := req.Context().(*TimingContext); ok {
		duration := tc.Duration()

		// Extract span attributes, including origin IP
		spanAttributes := p.extractSpanAttributes(req)

		// Send the span to Datadog
		p.sendCustomSpanWithDuration(traceID, spanID, duration, spanAttributes)
	}
}

// extractSpanAttributes extracts attributes from the request and global tags
func (p *TracingPlugin) extractSpanAttributes(req *http.Request) map[string]string {
	attributes := map[string]string{}

	// Add global tags
	for key, value := range p.globalTags {
		attributes[key] = value
	}

	// Extract HTTP-specific attributes
	attributes["http.method"] = req.Method
	attributes["http.url"] = req.URL.Path
	attributes["http.host"] = req.Host

	// Extract the origin IP from X-Forwarded-For or X-Real-Ip
	attributes["origin_ip"] = p.extractOriginIP(req)

	// Add static attributes
	attributes["language"] = "go"
	attributes["span.kind"] = "client"

	return attributes
}

// extractOriginIP checks for X-Forwarded-For and falls back to X-Real-Ip
func (p *TracingPlugin) extractOriginIP(req *http.Request) string {
	xForwardedFor := req.Header.Get("X-Forwarded-For")
	if xForwardedFor != "" {
		// Use only the leftmost IP in the chain
		if commaIndex := strings.Index(xForwardedFor, ","); commaIndex > 0 {
			return strings.TrimSpace(xForwardedFor[:commaIndex])
		}
		return strings.TrimSpace(xForwardedFor)
	}

	// Fallback to X-Real-Ip if X-Forwarded-For is missing
	return req.Header.Get("X-Real-Ip")
}

// createTraceAndSpan generates unique IDs for trace and span
func createTraceAndSpan() (uint64, uint64) {
	traceID := uint64(time.Now().UnixNano()) // Example trace ID
	spanID := traceID + 1                    // Example span ID
	return traceID, spanID
}

// injectTraceHeaders adds tracing headers to the request
func injectTraceHeaders(req *http.Request, traceID, spanID uint64) {
	req.Header.Set("x-datadog-trace-id", strconv.FormatUint(traceID, 10))
	req.Header.Set("x-datadog-parent-id", strconv.FormatUint(spanID, 10))
	req.Header.Set("x-datadog-sampling-priority", "1") // Sampling priority
}

// sendCustomSpanWithDuration sends a custom span directly to Datadog
func (p *TracingPlugin) sendCustomSpanWithDuration(traceID, spanID uint64, duration time.Duration, meta map[string]string) {
	span := map[string]interface{}{
		"trace_id":  traceID,
		"span_id":   spanID,
		"parent_id": 0,
		"name":      p.pluginName + "-operation",
		"resource":  p.pluginName + "-operation",
		"service":   p.pluginName + "-service",
		"start":     time.Now().Add(-duration).UnixNano(),
		"duration":  duration.Nanoseconds(),
		"meta":      meta,
	}

	payload, err := json.Marshal(span)
	if err != nil {
		log.Printf("Error serializing span: %v", err)
		return
	}

	trace := [][]map[string]interface{}{
		{span},
	}

	payload, err = json.Marshal(trace)
	if err != nil {
		log.Printf("Error serializing trace: %v", err)
		return
	}

	resp, err := http.Post(p.datadogTracingAgentUrl+"/v0.4/traces", "application/json", bytes.NewBuffer(payload))
	if err != nil {
		log.Printf("Error sending trace to Datadog: %v", err)
		return
	}
	defer resp.Body.Close()
}
