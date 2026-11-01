package logs

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/ymg4798/load-testing/internal/run"
)

// OTLP/HTTP logs (POST /v1/logs), protobuf or JSON, optionally gzipped.
// The protobuf is read field by field with protowire, like the trace
// decoder in internal/apm, so no generated code is needed.

const (
	maxBody         = 16 << 20
	maxBodyInflated = 64 << 20
)

// OTLPHandler serves POST /v1/logs into the store (whatever the path, so
// it can be mounted anywhere).
func (s *Store) OTLPHandler() http.Handler { return otlpHandler{s} }

type otlpHandler struct{ s *Store }

func (h otlpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	b, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ct := r.Header.Get("Content-Type")
	isJSON := strings.Contains(ct, "json")
	if !isJSON && !strings.Contains(ct, "protobuf") {
		t := strings.TrimLeft(string(b[:min(len(b), 16)]), " \t\r\n")
		isJSON = strings.HasPrefix(t, "{")
	}
	var lines []run.LogLine
	if isJSON {
		lines, err = DecodeOTLPJSON(b)
	} else {
		lines, err = DecodeOTLPProto(b)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.s.AddBatch(lines)
	// An empty ExportLogsServiceResponse means full success.
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func readBody(r *http.Request) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(nil, r.Body, maxBody)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		rd = io.LimitReader(zr, maxBodyInflated+1)
	}
	b, err := io.ReadAll(rd)
	if err == nil && len(b) > maxBodyInflated {
		err = errors.New("body too large")
	}
	return b, err
}

// Server is a standalone OTLP logs listener, for when APM is off.
type Server struct {
	ln  net.Listener
	srv *http.Server
}

// Serve listens on addr (such as ":4318") for POST /v1/logs into st.
func Serve(addr string, st *Store) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/logs", st.OTLPHandler())
	s := &Server{ln: ln, srv: &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Addr is the address the server listens on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops listening.
func (s *Server) Close() { _ = s.srv.Close() }

// ---- record assembly (shared by protobuf and JSON) ----

type otlpRecord struct {
	timeNs, observedNs int64
	sevNum             int
	sevText            string
	body               string
	traceID            string
	attrs              map[string]string
	eventName          string
}

// line maps a record: exception.* attributes become "stack" and "error",
// other attributes are kept up to maxAttrs.
func (o *otlpRecord) line(service string) run.LogLine {
	l := run.LogLine{Source: service, Message: o.body, TraceID: o.traceID}
	switch {
	case o.timeNs > 0:
		l.Time = time.Unix(0, o.timeNs)
	case o.observedNs > 0:
		l.Time = time.Unix(0, o.observedNs)
	}
	l.Level = normLevel(o.sevText)
	if l.Level == "" {
		l.Level = severityLevel(o.sevNum)
	}
	if len(o.attrs) > 0 {
		l.Attrs = map[string]string{}
		if st := o.attrs["exception.stacktrace"]; st != "" {
			l.Attrs["stack"] = truncUTF8(st, maxStack)
		}
		et, em := o.attrs["exception.type"], o.attrs["exception.message"]
		if em != "" || et != "" {
			l.Attrs["error"] = strings.TrimPrefix(et+": "+em, ": ")
			if l.Level == "" {
				l.Level = "error"
			}
		}
		if l.Message == "" {
			l.Message = l.Attrs["error"]
		}
		keys := make([]string, 0, len(o.attrs))
		for k := range o.attrs {
			if k != "exception.stacktrace" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			if len(l.Attrs) >= maxAttrs {
				break
			}
			l.Attrs[k] = truncUTF8(o.attrs[k], maxAttrValue)
		}
		if l.TraceID == "" {
			if t := o.attrs["trace_id"]; t != "" {
				l.TraceID = traceOf(t)
			}
		}
	}
	if l.Message == "" {
		l.Message = o.eventName
	}
	l.Message = truncUTF8(l.Message, maxMessage)
	l.TraceID = strings.ToLower(l.TraceID)
	return l
}

// ---- protobuf ----

var errTruncated = errors.New("otlp: truncated protobuf")

// fields walks one message's fields (as in internal/apm).
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

// DecodeOTLPProto reads an ExportLogsServiceRequest.
func DecodeOTLPProto(b []byte) ([]run.LogLine, error) {
	var out []run.LogLine
	err := fields(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType { // resource_logs
			return nil
		}
		var err error
		out, err = decodeResourceLogs(v, out)
		return err
	})
	return out, err
}

func decodeResourceLogs(b []byte, out []run.LogLine) ([]run.LogLine, error) {
	var service string
	var scopes [][]byte
	err := fields(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1: // resource
			return fields(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
				if num == 1 && typ == protowire.BytesType {
					k, val, err := decodeKeyValue(v, 0)
					if err == nil && k == "service.name" {
						service = val
					}
					return err
				}
				return nil
			})
		case 2, 1000: // scope_logs, deprecated instrumentation_library_logs
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
			if num != 2 || typ != protowire.BytesType { // log_records
				return nil
			}
			r, err := decodeLogRecord(v)
			if err != nil {
				return err
			}
			out = append(out, r.line(service))
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func decodeLogRecord(b []byte) (otlpRecord, error) {
	var r otlpRecord
	err := fields(b, func(num protowire.Number, typ protowire.Type, v []byte, u uint64) error {
		switch num {
		case 1:
			r.timeNs = int64(u)
		case 11:
			r.observedNs = int64(u)
		case 2:
			r.sevNum = int(u)
		case 3:
			r.sevText = string(v)
		case 5:
			if typ == protowire.BytesType {
				s, err := anyValueText(v, 0)
				if err != nil {
					return err
				}
				r.body = s
			}
		case 6:
			if typ == protowire.BytesType {
				k, val, err := decodeKeyValue(v, 0)
				if err != nil {
					return err
				}
				if r.attrs == nil {
					r.attrs = map[string]string{}
				}
				r.attrs[k] = val
			}
		case 9:
			if len(v) > 0 {
				r.traceID = hex.EncodeToString(v)
			}
		case 12:
			r.eventName = string(v)
		}
		return nil
	})
	return r, err
}

func decodeKeyValue(b []byte, depth int) (key, val string, err error) {
	err = fields(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		switch num {
		case 1:
			key = string(v)
		case 2:
			if typ == protowire.BytesType {
				s, err := anyValueText(v, depth)
				val = s
				return err
			}
		}
		return nil
	})
	return key, val, err
}

// anyValue decodes an AnyValue into a Go value (string, bool, int64,
// float64, []any, map[string]any).
func anyValue(b []byte, depth int) (any, error) {
	var out any
	if depth > 8 {
		return "…", nil
	}
	err := fields(b, func(num protowire.Number, typ protowire.Type, v []byte, u uint64) error {
		switch num {
		case 1:
			out = string(v)
		case 2:
			out = u != 0
		case 3:
			out = int64(u)
		case 4:
			out = math.Float64frombits(u)
		case 5: // ArrayValue
			list := []any{}
			err := fields(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
				if num == 1 && typ == protowire.BytesType {
					x, err := anyValue(v, depth+1)
					list = append(list, x)
					return err
				}
				return nil
			})
			out = list
			return err
		case 6: // KeyValueList
			m := map[string]any{}
			err := fields(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
				if num != 1 || typ != protowire.BytesType {
					return nil
				}
				var k string
				var x any
				err := fields(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
					switch num {
					case 1:
						k = string(v)
					case 2:
						var err error
						x, err = anyValue(v, depth+1)
						return err
					}
					return nil
				})
				m[k] = x
				return err
			})
			out = m
			return err
		case 7:
			out = base64.StdEncoding.EncodeToString(v)
		}
		return nil
	})
	return out, err
}

func anyValueText(b []byte, depth int) (string, error) {
	v, err := anyValue(b, depth)
	return valueText(v), err
}

// valueText renders a value: strings as they are, the rest as JSON.
func valueText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// ---- JSON ----

type jsonRequest struct {
	ResourceLogs  []jsonResourceLogs `json:"resourceLogs"`
	ResourceLogs2 []jsonResourceLogs `json:"resource_logs"`
}

type jsonResourceLogs struct {
	Resource struct {
		Attributes []jsonKV `json:"attributes"`
	} `json:"resource"`
	ScopeLogs  []jsonScopeLogs `json:"scopeLogs"`
	ScopeLogs2 []jsonScopeLogs `json:"scope_logs"`
	ILLogs     []jsonScopeLogs `json:"instrumentationLibraryLogs"`
}

type jsonScopeLogs struct {
	LogRecords  []jsonLogRecord `json:"logRecords"`
	LogRecords2 []jsonLogRecord `json:"log_records"`
}

type jsonLogRecord struct {
	Time          jsonInt    `json:"timeUnixNano"`
	Time2         jsonInt    `json:"time_unix_nano"`
	Observed      jsonInt    `json:"observedTimeUnixNano"`
	Observed2     jsonInt    `json:"observed_time_unix_nano"`
	SevNum        jsonSev    `json:"severityNumber"`
	SevNum2       jsonSev    `json:"severity_number"`
	SevText       string     `json:"severityText"`
	SevText2      string     `json:"severity_text"`
	Body          *jsonValue `json:"body"`
	Attributes    []jsonKV   `json:"attributes"`
	TraceID       string     `json:"traceId"`
	TraceID2      string     `json:"trace_id"`
	EventName     string     `json:"eventName"`
	EventName2    string     `json:"event_name"`
	DroppedAttrs  int        `json:"droppedAttributesCount"`
	DroppedAttrs2 int        `json:"dropped_attributes_count"`
}

type jsonKV struct {
	Key   string    `json:"key"`
	Value jsonValue `json:"value"`
}

type jsonValue struct {
	String  *string   `json:"stringValue"`
	String2 *string   `json:"string_value"`
	Bool    *bool     `json:"boolValue"`
	Bool2   *bool     `json:"bool_value"`
	Int     *jsonInt  `json:"intValue"`
	Int2    *jsonInt  `json:"int_value"`
	Double  *float64  `json:"doubleValue"`
	Double2 *float64  `json:"double_value"`
	Array   *jsonList `json:"arrayValue"`
	Array2  *jsonList `json:"array_value"`
	KVList  *jsonKVs  `json:"kvlistValue"`
	KVList2 *jsonKVs  `json:"kvlist_value"`
	Bytes   *string   `json:"bytesValue"`
	Bytes2  *string   `json:"bytes_value"`
}

type jsonList struct {
	Values []jsonValue `json:"values"`
}

type jsonKVs struct {
	Values []jsonKV `json:"values"`
}

func (v *jsonValue) value() any {
	switch {
	case v == nil:
		return nil
	case v.String != nil:
		return *v.String
	case v.String2 != nil:
		return *v.String2
	case v.Bool != nil:
		return *v.Bool
	case v.Bool2 != nil:
		return *v.Bool2
	case v.Int != nil:
		return int64(*v.Int)
	case v.Int2 != nil:
		return int64(*v.Int2)
	case v.Double != nil:
		return *v.Double
	case v.Double2 != nil:
		return *v.Double2
	case v.Bytes != nil:
		return *v.Bytes
	case v.Bytes2 != nil:
		return *v.Bytes2
	}
	if l := firstPtr(v.Array, v.Array2); l != nil {
		out := make([]any, len(l.Values))
		for i := range l.Values {
			out[i] = l.Values[i].value()
		}
		return out
	}
	if kv := firstPtr(v.KVList, v.KVList2); kv != nil {
		out := make(map[string]any, len(kv.Values))
		for i := range kv.Values {
			out[kv.Values[i].Key] = kv.Values[i].Value.value()
		}
		return out
	}
	return nil
}

func firstPtr[T any](a, b *T) *T {
	if a != nil {
		return a
	}
	return b
}

func first[T comparable](a, b T) T {
	var zero T
	if a != zero {
		return a
	}
	return b
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

// jsonSev is a SeverityNumber as a number or its name ("SEVERITY_NUMBER_ERROR").
type jsonSev int

var sevNames = map[string]int{
	"TRACE": 1, "TRACE2": 2, "TRACE3": 3, "TRACE4": 4,
	"DEBUG": 5, "DEBUG2": 6, "DEBUG3": 7, "DEBUG4": 8,
	"INFO": 9, "INFO2": 10, "INFO3": 11, "INFO4": 12,
	"WARN": 13, "WARN2": 14, "WARN3": 15, "WARN4": 16,
	"ERROR": 17, "ERROR2": 18, "ERROR3": 19, "ERROR4": 20,
	"FATAL": 21, "FATAL2": 22, "FATAL3": 23, "FATAL4": 24,
}

func (e *jsonSev) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if v, ok := sevNames[strings.TrimPrefix(s, "SEVERITY_NUMBER_")]; ok {
			*e = jsonSev(v)
			return nil
		}
		v, _ := strconv.Atoi(s)
		*e = jsonSev(v)
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*e = jsonSev(v)
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

// DecodeOTLPJSON reads an ExportLogsServiceRequest in OTLP/JSON.
func DecodeOTLPJSON(b []byte) ([]run.LogLine, error) {
	var req jsonRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("otlp json: %w", err)
	}
	var out []run.LogLine
	for _, rl := range append(req.ResourceLogs, req.ResourceLogs2...) {
		service := "unknown_service"
		for i := range rl.Resource.Attributes {
			if rl.Resource.Attributes[i].Key == "service.name" {
				service = valueText(rl.Resource.Attributes[i].Value.value())
			}
		}
		for _, sl := range append(append(rl.ScopeLogs, rl.ScopeLogs2...), rl.ILLogs...) {
			for _, jr := range append(sl.LogRecords, sl.LogRecords2...) {
				r := otlpRecord{
					timeNs:     int64(first(jr.Time, jr.Time2)),
					observedNs: int64(first(jr.Observed, jr.Observed2)),
					sevNum:     int(first(jr.SevNum, jr.SevNum2)),
					sevText:    first(jr.SevText, jr.SevText2),
					traceID:    jsonID(first(jr.TraceID, jr.TraceID2)),
					eventName:  first(jr.EventName, jr.EventName2),
				}
				if jr.Body != nil {
					r.body = valueText(jr.Body.value())
				}
				for i := range jr.Attributes {
					if r.attrs == nil {
						r.attrs = map[string]string{}
					}
					r.attrs[jr.Attributes[i].Key] = valueText(jr.Attributes[i].Value.value())
				}
				out = append(out, r.line(service))
			}
		}
	}
	return out, nil
}
