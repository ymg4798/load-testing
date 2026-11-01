package apm

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// rawSpan is one span as decoded from OTLP, before kind mapping.
type rawSpan struct {
	service   string
	traceID   string // hex
	spanID    string // hex
	parentID  string // hex, "" for a root
	name      string
	kind      int // OTel SpanKind: 1 internal, 2 server, 3 client, 4 producer, 5 consumer
	start     int64
	end       int64 // unix nanos
	attrs     map[string]string
	errored   bool // status code ERROR
	statusMsg string
	exc       *rawException // the first "exception" event
}

// rawException is a span's "exception" event.
type rawException struct {
	message, typ, stack string
	escaped             bool
}

func (e *rawException) set(k, v string) {
	switch k {
	case "exception.message":
		e.message = v
	case "exception.type":
		e.typ = v
	case "exception.stacktrace":
		e.stack = v
	case "exception.escaped":
		e.escaped = v == "true"
	}
}

// OTLP enum values.
const (
	kindInternal = 1
	kindServer   = 2
	kindClient   = 3
	kindProducer = 4
	kindConsumer = 5

	statusError = 2
)

// keptAttrs are the span attributes worth keeping; the rest are dropped.
var keptAttrs = map[string]bool{
	"http.method": true, "http.request.method": true,
	"http.route": true, "url.path": true, "http.target": true, "http.url": true, "url.full": true,
	"http.status_code": true, "http.response.status_code": true,
	"db.system": true, "db.statement": true, "db.query.text": true, "db.name": true, "db.namespace": true,
	"db.operation": true, "db.operation.name": true,
	"peer.service": true, "server.address": true, "server.port": true, "net.peer.name": true, "net.peer.port": true,
	"http.host": true, "messaging.system": true, "messaging.destination.name": true,
	"rpc.system": true, "rpc.service": true, "rpc.method": true,
	"exception.message": true, "exception.type": true, "exception.stacktrace": true, "error.type": true,
	"lt.stack": true,
}

const (
	maxStatement = 2048
	maxStack     = 8 << 10 // exception.stacktrace, lt.stack
	maxMessage   = 1024    // exception.message
)

func keepAttr(m map[string]string, k, v string) map[string]string {
	if !keptAttrs[k] {
		return m
	}
	if m == nil {
		m = make(map[string]string, 4)
	}
	switch k {
	case "db.statement", "db.query.text":
		v = truncUTF8(v, maxStatement)
	case "exception.stacktrace", "lt.stack":
		v = truncUTF8(v, maxStack)
	case "exception.message":
		v = truncUTF8(v, maxMessage)
	}
	m[k] = v
	return m
}

func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// ---- protobuf ----

var errTruncated = errors.New("otlp: truncated protobuf")

// fields walks the fields of one message, calling f with each field's number,
// wire type and raw value (for bytes: the content; for varints and fixed: the
// encoded value bytes, decoded with the helpers below).
func fields(b []byte, f func(num protowire.Number, typ protowire.Type, v []byte, u uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return errTruncated
		}
		b = b[n:]
		var (
			v []byte
			u uint64
		)
		switch typ {
		case protowire.VarintType:
			u, n = protowire.ConsumeVarint(b)
		case protowire.Fixed64Type:
			u, n = protowire.ConsumeFixed64(b)
		case protowire.Fixed32Type:
			var u32 uint32
			u32, n = protowire.ConsumeFixed32(b)
			u = uint64(u32)
		case protowire.BytesType:
			v, n = protowire.ConsumeBytes(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return errTruncated
		}
		b = b[n:]
		if err := f(num, typ, v, u); err != nil {
			return err
		}
	}
	return nil
}

// decodeProto reads an ExportTraceServiceRequest.
func decodeProto(b []byte) ([]rawSpan, error) {
	var out []rawSpan
	err := fields(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType { // resource_spans
			return nil
		}
		var err error
		out, err = decodeResourceSpans(v, out)
		return err
	})
	return out, err
}

func decodeResourceSpans(b []byte, out []rawSpan) ([]rawSpan, error) {
	var service string
	var scopes [][]byte
	err := fields(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1: // resource
			return fields(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
				if num == 1 && typ == protowire.BytesType { // attributes
					k, val, err := decodeKeyValue(v)
					if err == nil && k == "service.name" {
						service = val
					}
					return err
				}
				return nil
			})
		case 2, 1000: // scope_spans, deprecated instrumentation_library_spans
			scopes = append(scopes, v)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if service == "" {
		service = "unknown_service"
	}
	for _, sc := range scopes {
		err := fields(sc, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
			if num != 2 || typ != protowire.BytesType { // spans
				return nil
			}
			s, err := decodeSpan(v)
			if err != nil {
				return err
			}
			s.service = service
			out = append(out, s)
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func decodeSpan(b []byte) (rawSpan, error) {
	var s rawSpan
	err := fields(b, func(num protowire.Number, typ protowire.Type, v []byte, u uint64) error {
		switch num {
		case 1:
			s.traceID = hex.EncodeToString(v)
		case 2:
			s.spanID = hex.EncodeToString(v)
		case 4:
			if len(v) > 0 {
				s.parentID = hex.EncodeToString(v)
			}
		case 5:
			s.name = string(v)
		case 6:
			s.kind = int(u)
		case 7:
			s.start = int64(u)
		case 8:
			s.end = int64(u)
		case 9:
			if typ == protowire.BytesType {
				k, val, err := decodeKeyValue(v)
				if err != nil {
					return err
				}
				s.attrs = keepAttr(s.attrs, k, val)
			}
		case 11: // events
			if typ == protowire.BytesType && s.exc == nil {
				e, err := decodeEvent(v)
				if err != nil {
					return err
				}
				s.exc = e
			}
		case 15: // status
			if typ == protowire.BytesType {
				return fields(v, func(num protowire.Number, _ protowire.Type, v []byte, u uint64) error {
					switch num {
					case 2:
						s.statusMsg = string(v)
					case 3:
						s.errored = u == statusError
					}
					return nil
				})
			}
		}
		return nil
	})
	return s, err
}

// decodeEvent returns the exception in a span event, or nil for other
// events (without allocating for those).
func decodeEvent(b []byte) (*rawException, error) {
	isExc := false
	err := fields(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num == 2 && typ == protowire.BytesType {
			isExc = string(v) == "exception"
		}
		return nil
	})
	if err != nil || !isExc {
		return nil, err
	}
	e := &rawException{}
	err = fields(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num == 3 && typ == protowire.BytesType {
			k, val, err := decodeKeyValue(v)
			if err != nil {
				return err
			}
			e.set(k, val)
		}
		return nil
	})
	return e, err
}

// decodeKeyValue returns a KeyValue's key and its value as text.
func decodeKeyValue(b []byte) (key, val string, err error) {
	err = fields(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		switch num {
		case 1:
			key = string(v)
		case 2: // AnyValue
			return fields(v, func(num protowire.Number, _ protowire.Type, v []byte, u uint64) error {
				switch num {
				case 1:
					val = string(v)
				case 2:
					val = strconv.FormatBool(u != 0)
				case 3:
					val = strconv.FormatInt(int64(u), 10)
				case 4:
					val = strconv.FormatFloat(math.Float64frombits(u), 'g', -1, 64)
				}
				return nil
			})
		}
		return nil
	})
	return key, val, err
}

// ---- JSON ----
// OTLP/JSON uses lowerCamelCase names; receivers must also accept the
// original snake_case names, hence the doubled fields.

type jsonRequest struct {
	ResourceSpans  []jsonResourceSpans `json:"resourceSpans"`
	ResourceSpans2 []jsonResourceSpans `json:"resource_spans"`
}

type jsonResourceSpans struct {
	Resource struct {
		Attributes []jsonKV `json:"attributes"`
	} `json:"resource"`
	ScopeSpans  []jsonScopeSpans `json:"scopeSpans"`
	ScopeSpans2 []jsonScopeSpans `json:"scope_spans"`
	ILSpans     []jsonScopeSpans `json:"instrumentationLibrarySpans"`
}

type jsonScopeSpans struct {
	Spans []jsonSpan `json:"spans"`
}

type jsonSpan struct {
	TraceID       string      `json:"traceId"`
	TraceID2      string      `json:"trace_id"`
	SpanID        string      `json:"spanId"`
	SpanID2       string      `json:"span_id"`
	ParentSpanID  string      `json:"parentSpanId"`
	ParentSpanID2 string      `json:"parent_span_id"`
	Name          string      `json:"name"`
	Kind          jsonEnum    `json:"kind"`
	Start         jsonInt     `json:"startTimeUnixNano"`
	Start2        jsonInt     `json:"start_time_unix_nano"`
	End           jsonInt     `json:"endTimeUnixNano"`
	End2          jsonInt     `json:"end_time_unix_nano"`
	Attributes    []jsonKV    `json:"attributes"`
	Events        []jsonEvent `json:"events"`
	Status        *jsonStat   `json:"status"`
}

type jsonEvent struct {
	Name       string   `json:"name"`
	Attributes []jsonKV `json:"attributes"`
}

type jsonStat struct {
	Code    jsonEnum `json:"code"`
	Message string   `json:"message"`
}

type jsonKV struct {
	Key   string `json:"key"`
	Value struct {
		String  *string  `json:"stringValue"`
		String2 *string  `json:"string_value"`
		Bool    *bool    `json:"boolValue"`
		Bool2   *bool    `json:"bool_value"`
		Int     *jsonInt `json:"intValue"`
		Int2    *jsonInt `json:"int_value"`
		Double  *float64 `json:"doubleValue"`
		Double2 *float64 `json:"double_value"`
	} `json:"value"`
}

func (kv *jsonKV) text() string {
	v := &kv.Value
	switch {
	case v.String != nil:
		return *v.String
	case v.String2 != nil:
		return *v.String2
	case v.Bool != nil:
		return strconv.FormatBool(*v.Bool)
	case v.Bool2 != nil:
		return strconv.FormatBool(*v.Bool2)
	case v.Int != nil:
		return strconv.FormatInt(int64(*v.Int), 10)
	case v.Int2 != nil:
		return strconv.FormatInt(int64(*v.Int2), 10)
	case v.Double != nil:
		return strconv.FormatFloat(*v.Double, 'g', -1, 64)
	case v.Double2 != nil:
		return strconv.FormatFloat(*v.Double2, 'g', -1, 64)
	}
	return ""
}

// jsonInt is an int64 written as a JSON string (the OTLP rule) or number.
type jsonInt int64

func (j *jsonInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*j = jsonInt(v)
		return nil
	}
	if v, err := strconv.ParseUint(s, 10, 64); err == nil {
		*j = jsonInt(int64(v))
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("otlp: bad integer %s", b)
	}
	*j = jsonInt(int64(f))
	return nil
}

// jsonEnum is an enum written as a number or as its name ("SPAN_KIND_SERVER").
type jsonEnum int

var enumNames = map[string]int{
	"SPAN_KIND_UNSPECIFIED": 0, "SPAN_KIND_INTERNAL": 1, "SPAN_KIND_SERVER": 2,
	"SPAN_KIND_CLIENT": 3, "SPAN_KIND_PRODUCER": 4, "SPAN_KIND_CONSUMER": 5,
	"STATUS_CODE_UNSET": 0, "STATUS_CODE_OK": 1, "STATUS_CODE_ERROR": 2,
}

func (e *jsonEnum) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if v, ok := enumNames[s]; ok {
			*e = jsonEnum(v)
			return nil
		}
		v, _ := strconv.Atoi(s)
		*e = jsonEnum(v)
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*e = jsonEnum(v)
	return nil
}

// jsonID turns an OTLP/JSON ID (hex; some old exporters wrote base64) into hex.
func jsonID(s string) string {
	if s == "" {
		return ""
	}
	if _, err := hex.DecodeString(s); err == nil {
		return strings.ToLower(s)
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return hex.EncodeToString(b)
	}
	return s
}

func first[T comparable](a, b T) T {
	var zero T
	if a != zero {
		return a
	}
	return b
}

func decodeJSON(b []byte) ([]rawSpan, error) {
	var req jsonRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("otlp json: %w", err)
	}
	var out []rawSpan
	for _, rs := range append(req.ResourceSpans, req.ResourceSpans2...) {
		service := "unknown_service"
		for i := range rs.Resource.Attributes {
			if rs.Resource.Attributes[i].Key == "service.name" {
				service = rs.Resource.Attributes[i].text()
			}
		}
		for _, ss := range append(append(rs.ScopeSpans, rs.ScopeSpans2...), rs.ILSpans...) {
			for i := range ss.Spans {
				js := &ss.Spans[i]
				s := rawSpan{
					service:  service,
					traceID:  jsonID(first(js.TraceID, js.TraceID2)),
					spanID:   jsonID(first(js.SpanID, js.SpanID2)),
					parentID: jsonID(first(js.ParentSpanID, js.ParentSpanID2)),
					name:     js.Name,
					kind:     int(js.Kind),
					start:    int64(first(js.Start, js.Start2)),
					end:      int64(first(js.End, js.End2)),
				}
				for j := range js.Attributes {
					s.attrs = keepAttr(s.attrs, js.Attributes[j].Key, js.Attributes[j].text())
				}
				if js.Status != nil {
					s.errored = js.Status.Code == statusError
					s.statusMsg = js.Status.Message
				}
				for j := range js.Events {
					if ev := &js.Events[j]; ev.Name == "exception" {
						s.exc = &rawException{}
						for k := range ev.Attributes {
							s.exc.set(ev.Attributes[k].Key, ev.Attributes[k].text())
						}
						break
					}
				}
				out = append(out, s)
			}
		}
	}
	return out, nil
}
