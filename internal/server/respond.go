package server

import (
	"bytes"
	"compress/gzip"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"airview/internal/analytics"
)

// httpError is an error with an HTTP status for the client.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func errStatus(status int, format string, args ...any) error {
	return &httpError{status: status, msg: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error {
	return errStatus(http.StatusBadRequest, format, args...)
}

func notFound(format string, args ...any) error {
	return errStatus(http.StatusNotFound, format, args...)
}

// statusResult is an API result with a success status other than 200
// (uncached routes only).
type statusResult struct {
	status int
	v      any
}

// accepted wraps v as a 202 Accepted result.
func accepted(v any) statusResult { return statusResult{status: http.StatusAccepted, v: v} }

// num is a float that encodes NaN / Inf as null.
type num float64

func (n num) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, f, 'f', -1, 64), nil
}

// rnd rounds to d decimals and wraps the value as a num.
func rnd(v float64, d int) num {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return num(math.NaN())
	}
	p := math.Pow(10, float64(d))
	return num(math.Round(v*p) / p)
}

// nums converts a slice, rounding to d decimals.
func nums(v []float64, d int) []num {
	out := make([]num, len(v))
	for i, x := range v {
		out[i] = rnd(x, d)
	}
	return out
}

// marshal encodes v. encoding/json refuses NaN and Inf; the core packages
// keep them out of their result types, but a value that slips through must
// not turn a whole response into an error, so in that case the value is
// re-encoded through a sanitising walk that writes null instead.
func marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err == nil {
		return b, nil
	}
	if !isUnsupportedValue(err) {
		return nil, err
	}
	return json.Marshal(sanitize(reflect.ValueOf(v), 0))
}

func isUnsupportedValue(err error) bool {
	var uv *json.UnsupportedValueError
	if errors.As(err, &uv) {
		return true
	}
	var me *json.MarshalerError
	return errors.As(err, &me) && strings.Contains(err.Error(), "unsupported value")
}

var (
	marshalerType     = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// sanitize converts a value into plain maps / slices / scalars the way
// encoding/json would see it (json tags, omitempty, embedded structs,
// Marshaler types), replacing non-finite floats by nil.
func sanitize(v reflect.Value, depth int) any {
	if !v.IsValid() || depth > 64 {
		return nil
	}
	t := v.Type()
	if t.Implements(marshalerType) {
		if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
			return nil
		}
		if b, err := v.Interface().(json.Marshaler).MarshalJSON(); err == nil && json.Valid(b) {
			return json.RawMessage(b)
		}
		if v.Kind() != reflect.Struct && v.Kind() != reflect.Pointer {
			return nil
		}
	} else if t.Implements(textMarshalerType) && v.Kind() != reflect.Pointer {
		if b, err := v.Interface().(encoding.TextMarshaler).MarshalText(); err == nil {
			return string(b)
		}
	}
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return f
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return sanitize(v.Elem(), depth+1)
	case reflect.Struct:
		out := map[string]any{}
		sanitizeStruct(v, out, depth)
		return out
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		out := make(map[string]any, v.Len())
		it := v.MapRange()
		for it.Next() {
			out[fmt.Sprint(it.Key().Interface())] = sanitize(it.Value(), depth+1)
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		if t.Elem().Kind() == reflect.Uint8 {
			return v.Interface() // []byte / json.RawMessage
		}
		fallthrough
	case reflect.Array:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = sanitize(v.Index(i), depth+1)
		}
		return out
	case reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128:
		return nil
	}
	return v.Interface()
}

func sanitizeStruct(v reflect.Value, out map[string]any, depth int) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		fv := v.Field(i)
		if f.Anonymous && name == "" {
			ev := fv
			if ev.Kind() == reflect.Pointer {
				if ev.IsNil() {
					continue
				}
				ev = ev.Elem()
			}
			if ev.Kind() == reflect.Struct && !ev.Type().Implements(marshalerType) {
				sanitizeStruct(ev, out, depth+1)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if strings.Contains(opts, "omitempty") && fv.IsZero() {
			continue
		}
		out[name] = sanitize(fv, depth+1)
	}
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

const jsonType = "application/json; charset=utf-8"

// gzipMin is the smallest body worth compressing.
const gzipMin = 1024

var gzPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return w
}}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(b)/4 + 64)
	zw := gzPool.Get().(*gzip.Writer)
	zw.Reset(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	gzPool.Put(zw)
	return buf.Bytes()
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") {
			q := strings.TrimPrefix(strings.ReplaceAll(params, " ", ""), "q=")
			if f, err := strconv.ParseFloat(q, 64); err == nil && f == 0 {
				return false // explicitly refused
			}
			return true
		}
	}
	return false
}

// writeBody sends an encoded body, gzip-compressed when the client accepts
// it. gz may hold the already compressed form (nil = compress on demand).
func writeBody(w http.ResponseWriter, r *http.Request, status int, body, gz []byte, ctype string) {
	if ctype == "" {
		ctype = jsonType
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Add("Vary", "Accept-Encoding")
	if len(body) >= gzipMin && r != nil && acceptsGzip(r) {
		if gz == nil {
			gz = gzipBytes(body)
		}
		h.Set("Content-Encoding", "gzip")
		body = gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(apiTimeout))
	w.WriteHeader(status)
	if r == nil || r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	b, err := marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeBody(w, r, status, b, nil, "")
}

func writeError(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Encoding")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// readJSON decodes a small JSON request body.
func readJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Query parameters
// ---------------------------------------------------------------------------

func qStr(r *http.Request, name string) string {
	return strings.TrimSpace(r.URL.Query().Get(name))
}

func qInt(r *http.Request, name string, def, lo, hi int) (int, error) {
	s := qStr(r, name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, badRequest("parameter %s: not an integer", name)
	}
	return min(max(n, lo), hi), nil
}

func qInt64(r *http.Request, name string, def int64) (int64, error) {
	s := qStr(r, name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, badRequest("parameter %s: not an integer", name)
	}
	return n, nil
}

func qFloat(r *http.Request, name string, def float64) (float64, error) {
	s := qStr(r, name)
	if s == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, badRequest("parameter %s: not a number", name)
	}
	return f, nil
}

func qBool(r *http.Request, name string, def bool) bool {
	switch strings.ToLower(qStr(r, name)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// qAnt returns the antenna selector: "" (also "all" / "combined") = all
// antennas combined.
func qAnt(r *http.Request) (string, error) {
	a := qStr(r, "ant")
	if a == "all" || a == "combined" {
		return "", nil
	}
	if len(a) > 64 {
		return "", badRequest("parameter ant: too long")
	}
	return a, nil
}

// maxRangeDays caps the span a client may ask for.
const maxRangeDays = 3660

// qSpan parses the common time-range parameters: from / to (Unix ms),
// hours=N or days=N (ending at to or now). ok is false when none was given.
func qSpan(r *http.Request) (from, to time.Time, ok bool, err error) {
	fromMs, err := qInt64(r, "from", 0)
	if err != nil {
		return
	}
	toMs, err := qInt64(r, "to", 0)
	if err != nil {
		return
	}
	hours, err := qFloat(r, "hours", 0)
	if err != nil {
		return
	}
	days, err := qFloat(r, "days", 0)
	if err != nil {
		return
	}
	if fromMs < 0 || toMs < 0 || hours < 0 || days < 0 {
		err = badRequest("time range parameters must not be negative")
		return
	}
	if fromMs == 0 && toMs == 0 && hours == 0 && days == 0 {
		return
	}
	to = time.Now()
	if toMs > 0 {
		to = time.UnixMilli(toMs)
	}
	span := time.Duration((hours + days*24) * float64(time.Hour))
	switch {
	case fromMs > 0:
		from = time.UnixMilli(fromMs)
	case span > 0:
		from = to.Add(-span)
	default:
		err = badRequest("parameter to needs from, hours or days")
		return
	}
	if !from.Before(to) {
		err = badRequest("time range is empty: from must be before to")
		return
	}
	if to.Sub(from) > maxRangeDays*24*time.Hour {
		from = to.Add(-maxRangeDays * 24 * time.Hour)
	}
	return from, to, true, nil
}

// qSpanDefault is qSpan with a default span ending now.
func qSpanDefault(r *http.Request, def time.Duration) (from, to time.Time, err error) {
	from, to, ok, err := qSpan(r)
	if err != nil {
		return
	}
	if !ok {
		to = time.Now()
		from = to.Add(-def)
	}
	return from, to, nil
}

// qRange is qSpan as an analytics.Range; the zero Range (no parameter)
// selects the default span of the analysis.
func qRange(r *http.Request) (analytics.Range, error) {
	from, to, ok, err := qSpan(r)
	if err != nil || !ok {
		return analytics.Range{}, err
	}
	return analytics.Range{From: from.Unix(), To: to.Unix()}, nil
}

// cacheKey identifies a GET request independent of parameter order.
func cacheKey(r *http.Request) string {
	q := r.URL.Query()
	if len(q) == 0 {
		return r.URL.Path
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(r.URL.Path)
	for _, k := range keys {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strings.Join(q[k], ","))
	}
	if b.Len() > 512 {
		return "" // not cached
	}
	return b.String()
}
