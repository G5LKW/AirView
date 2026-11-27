package config

//go:generate go run ./gen

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"airview/internal/model"
)

// docOverrides replace or supply the documentation of settings whose Go doc
// comment is missing or written for programmers (keyed by generic path).
var docOverrides = map[string]string{
	"mode":                          `Run mode: "standalone" (local receiver, map and statistics), "aggregator" (multi-station network server, see the aggregator section), "remote-data" (desktop app fed from another AirView's hub port) or "remote-web" (desktop window showing another AirView's website).`,
	"http.adminPasswordHash":        `Admin password protecting settings changes from other computers. Stored as "sha256:<salt>:<hash>". In the operator file or AIRVIEW_HTTP_ADMIN_PASSWORD_HASH you may give "plain:<password>" instead; it is hashed when loaded and never written to config.json. "" = no password (changes allowed from everywhere).`,
	"receiver.name":                 "Display name of this station.",
	"receiver.lon":                  "Longitude in WGS84 degrees (east positive).",
	"receiver.lat":                  "Latitude in WGS84 degrees (north positive).",
	"antennas":                      "The physical antennas and their RF chains (at most 16).",
	"antennas[].frontToBackDb":      "Front-to-back ratio of a directional antenna in dB.",
	"antennas[].lnaGainDb":          "Gain of the low-noise amplifier in dB (0 = none).",
	"antennas[].lnaNoiseFigDb":      "Noise figure of the low-noise amplifier in dB.",
	"antennas[].filterLossDb":       "Insertion loss of filters before the first amplifier in dB.",
	"antennas[].notes":              "Free-text notes.",
	"feeds":                         "The data inputs (at most 32).",
	"demo.enabled":                  "Feed the built-in traffic simulator instead of (or besides) a receiver.",
	"aggregator.tlsKey":             "PEM private key file of an operator TLS certificate for AVF/1 (used together with tlsCert; \"\" = self-signed certificate in <data>/avf-tls/). Treated as a secret.",
	"aggregator.tlsCert":            "PEM certificate (chain) file of an operator TLS certificate for AVF/1 (used together with tlsKey; \"\" = self-signed ECDSA P-256 certificate in <data>/avf-tls/).",
	"aggregator.publicMap":          "Serve the public merged live map (default true).",
	"aggregator.publicStats":        "Serve the public network statistics (default true).",
	"aggregator.tarJson":            "Serve tar1090-shaped /data/aircraft.json and /data/receiver.json (default true).",
	"aggregator.limits.publicRps":   "Public API rate limit per client IP in requests per second (default 1, 0.1..1000).",
	"aggregator.limits.publicBurst": "Burst size of the public API rate limit per client IP (default 10, 1..10000).",
	"antennas[].name":               "Display name of the antenna.",
	"feeds[].name":                  "Display name of the feed.",
	"feeds[].enabled":               "Connect this feed (false keeps the entry without using it).",
	"feeds[].host":                  "Host name or IP address of the data source (Beast/AVR/SBS/hub feeds).",
	"feeds[].port":                  "TCP port of the data source; 0 = the usual port of the feed type (Beast 10003, AVR 10002, SBS 30003, hub 30154).",
	"feeds[].type":                  `Protocol: "beast", "avr", "sbs", "json" (HTTP polling of aircraft.json), "hub" (another AirView) or "demo" (simulator).`,
	"feeds[].notes":                 "Free-text notes.",
	"feeds[].clockClass":            `Overrides the detected receiver clock class sent on uplinks ("" = detect). One of "none", "gps_utc", "gps_gpst", "gps_emul", "hw12", "sdr12", "sdr12_hires", "sbs20".`,
	"ntp.enabled":                   "Sample an NTP server to measure the host clock offset (timing analysis).",
	"weather.enabled":               "Download free weather data (Open-Meteo, no API key) for the station location.",
	"hub.enabled":                   "Open the hub data port that other AirView instances (remote-data mode) log in to.",
	"uplinks[].pin":                 `Requested precision of this station's public map pin: "hidden" (default, no pin), "region" or "cell01".`,
	"uplinks[].spoolMb":             "Size of the on-disk spool written while the aggregator is unreachable, in MiB. 0 = RAM ring only; -1 = automatic (0 on an SD-card root, 256 on other Linux hosts, 1024 on Windows).",
	"aggregator.defaultPin":         `Public pin precision of new stations: "hidden" (default), "region" or "cell01".`,
	"aggregator.admission":          `How new stations are admitted: "open" (automatically), "approve" (held until the admin approves) or "invite" (default; an admin-minted enrol token is required).`,
	"aggregator.mlat.autoUseSurvey": "Use a reverse-MLAT (surveyed) station position for MLAT without owner confirmation when its horizontal sigma is at most autoUseMaxSigmaM and it passes the suspect checks. The position stays owner/admin-only.",
}

// sectionTitles name the top-level sections in docs/CONFIG.md.
var sectionTitles = map[string]string{
	"":           "General",
	"http":       "Web server",
	"receiver":   "Receiver",
	"antennas":   "Antennas",
	"feeds":      "Feeds",
	"ntp":        "NTP reference",
	"weather":    "Weather",
	"hub":        "Hub data port",
	"remote":     "Remote modes",
	"storage":    "Storage and retention",
	"ui":         "User interface defaults",
	"demo":       "Demo traffic",
	"prediction": "Trajectory prediction",
	"terrain":    "Terrain analysis",
	"navData":    "Aviation reference data",
	"alerts":     "Alerts",
	"rfStat":     "RF statistics",
	"uplinks":    "Uplinks (feeding an aggregator)",
	"aggregator": "Aggregator mode",
}

// FieldDoc returns the documentation of a setting (generic path such as
// "http.port" or "feeds[].host").
func FieldDoc(path string) string { return fieldDoc(modelDocs, path) }

func fieldDoc(docs map[string]string, path string) string {
	f, ok := FieldByPath(path)
	if !ok {
		return ""
	}
	if s, ok := docOverrides[f.Path]; ok {
		return s
	}
	goName := f.DocKey[strings.LastIndexByte(f.DocKey, '.')+1:]
	name := f.Path[strings.LastIndexAny(f.Path, ".]")+1:]
	return humanize(docs[f.DocKey], goName, name)
}

// typeDoc returns the doc comment of a section's Go type, phrased for users.
func typeDoc(docs map[string]string, t reflect.Type) string {
	return humanize(docs[t.Name()], t.Name(), "")
}

// humanize rewrites a Go doc comment for users: a leading Go identifier
// ("BindAddr is ...") becomes the JSON name (or is dropped for types) and the
// first letter is capitalised.
func humanize(doc, goName, jsonName string) string {
	doc = strings.TrimSpace(doc)
	if rest, ok := strings.CutPrefix(doc, goName+" "); ok && goName != "" {
		if jsonName != "" {
			doc = jsonName + " " + rest
		} else {
			doc = rest
		}
	}
	if doc != "" && doc[0] >= 'a' && doc[0] <= 'z' && (jsonName == "" || !strings.HasPrefix(doc, jsonName+" ")) {
		doc = strings.ToUpper(doc[:1]) + doc[1:]
	}
	if doc != "" && !strings.HasSuffix(doc, ".") && !strings.HasSuffix(doc, ")") {
		doc += "."
	}
	return doc
}

// EnvName returns the environment variable of a setting path; list indices
// are kept ("uplinks[0].host" -> "AIRVIEW_UPLINKS_0_HOST"). ok is false for
// unknown paths.
func EnvName(path string) (string, bool) {
	segs, ok := parsePath(path)
	if !ok {
		return "", false
	}
	if _, ok := typeAt(segs); !ok {
		return "", false
	}
	var b strings.Builder
	b.WriteString(strings.TrimSuffix(EnvPrefix, "_"))
	for _, s := range segs {
		b.WriteByte('_')
		if s.isIndex() {
			fmt.Fprint(&b, s.idx)
		} else {
			b.WriteString(envSegment(s.key))
		}
	}
	return b.String(), true
}

// ExampleFileName is the name of the generated example configuration.
const ExampleFileName = "airview.example.jsonc"

// ExampleJSONC returns the fully commented example configuration
// (airview.example.jsonc): every setting with its documentation, default
// value and environment variable. Parsed, it yields exactly the defaults.
func ExampleJSONC() string { return RenderExample(modelDocs) }

// RenderExample renders the example configuration with the given model
// documentation (the generator passes freshly extracted docs).
func RenderExample(docs map[string]string) string {
	w := &jsoncWriter{docs: docs}
	w.line(0, "// AirView configuration file (JSON with comments).")
	w.line(0, "//")
	w.line(0, "// This example lists every setting with its default value. Copy it to")
	w.line(0, "// airview.jsonc in the data directory, or point -config / AIRVIEW_CONFIG at")
	w.line(0, "// it (for example /etc/airview/airview.jsonc), and keep only the settings")
	w.line(0, "// you change: anything not listed keeps the value from config.json (the")
	w.line(0, "// file the web UI saves) or the default. Settings listed here override")
	w.line(0, "// config.json and are shown as locked in the UI; AirView never writes this")
	w.line(0, "// file. Every setting can also be given as an environment variable")
	w.line(0, "// (shown with each setting), which overrides this file.")
	w.line(0, "//")
	w.line(0, "// Comments (// and /* */) and trailing commas are allowed.")
	w.line(0, "// Check a file with:  airview config check -config <file>")
	w.line(0, "// Full reference:     docs/CONFIG.md")
	w.line(0, "{")
	def, _ := Validate(model.DefaultConfig())
	w.line(1, "// Layout version of the file (informational in this file).")
	w.line(1, fmt.Sprintf(`"version": %d,`, model.ConfigVersion))
	w.object(reflect.ValueOf(def), "", "AIRVIEW", 1, true)
	w.line(0, "}")
	return w.b.String()
}

type jsoncWriter struct {
	b    strings.Builder
	docs map[string]string
}

func (w *jsoncWriter) line(indent int, s string) {
	w.b.WriteString(strings.Repeat("  ", indent))
	w.b.WriteString(s)
	w.b.WriteByte('\n')
}

func (w *jsoncWriter) comment(indent int, text string) {
	for _, l := range wrap(text, 78-2*indent-3) {
		w.line(indent, "// "+l)
	}
}

// object writes the members of struct value v (without braces).
// withDocs=false writes the values only (second and later list elements).
func (w *jsoncWriter) object(v reflect.Value, prefix, env string, indent int, withDocs bool) {
	t := v.Type()
	var names []int
	for i := 0; i < t.NumField(); i++ {
		if n, ok := jsonName(t.Field(i)); ok && !(prefix == "" && n == "version") {
			names = append(names, i)
		}
	}
	for k, i := range names {
		sf := t.Field(i)
		name, _ := jsonName(sf)
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fenv := env + "_" + envSegment(name)
		fv := v.Field(i)
		comma := ","
		if k == len(names)-1 {
			comma = ""
		}
		if withDocs {
			w.fieldComment(indent, path, fenv, fv)
		}
		key, _ := json.Marshal(name)
		switch {
		case sf.Type.Kind() == reflect.Struct:
			w.line(indent, string(key)+": {")
			w.object(fv, path, fenv, indent+1, withDocs)
			w.line(indent, "}"+comma)
		case sf.Type.Kind() == reflect.Slice && sf.Type.Elem().Kind() == reflect.Struct:
			if fv.Len() == 0 {
				w.line(indent, string(key)+": [")
				w.exampleElement(path, fenv, indent+1)
				w.line(indent, "]"+comma)
				continue
			}
			w.line(indent, string(key)+": [")
			for j := 0; j < fv.Len(); j++ {
				w.line(indent+1, "{")
				w.object(fv.Index(j), path+"[]", fenv+"_"+fmt.Sprint(j), indent+2, withDocs && j == 0)
				sep := ","
				if j == fv.Len()-1 {
					sep = ""
				}
				w.line(indent+1, "}"+sep)
			}
			w.line(indent, "]"+comma)
		default:
			w.line(indent, string(key)+": "+scalarJSON(fv)+comma)
		}
	}
}

// fieldComment writes the documentation lines of one setting.
func (w *jsoncWriter) fieldComment(indent int, path, env string, v reflect.Value) {
	gp := genericPath(path)
	f, _ := FieldByPath(gp)
	if v.Kind() == reflect.Struct {
		if d := typeDoc(w.docs, v.Type()); d != "" {
			w.comment(indent, d)
		}
		return
	}
	if d := fieldDoc(w.docs, gp); d != "" {
		w.comment(indent, d)
	}
	var meta []string
	if f.Kind == KindStructList {
		meta = append(meta, "Environment: "+env+" (JSON list) or "+env+"_<n>_<FIELD> per element.")
	} else {
		meta = append(meta, "Environment: "+env+".")
	}
	if f.Secret {
		meta = append(meta, "Secret: redacted in the UI and by 'airview config print'; "+env+"_FILE reads it from a file.")
	}
	w.comment(indent, strings.Join(meta, " "))
}

// exampleElement writes a commented-out example element for an empty list.
func (w *jsoncWriter) exampleElement(path, env string, indent int) {
	var ex reflect.Value
	switch path {
	case "uplinks":
		u := model.DefaultUplink()
		u.ID, u.Host, u.EnrolToken = "club", "airview.example.org", "<enrol token from the aggregator admin>"
		ex = reflect.ValueOf(u)
	default:
		return
	}
	sub := &jsoncWriter{docs: w.docs}
	sub.line(0, "{")
	sub.object(ex, path+"[]", env+"_0", 1, true)
	sub.line(0, "}")
	w.line(indent, "// Example entry (remove the // to use it):")
	for _, l := range strings.Split(strings.TrimSuffix(sub.b.String(), "\n"), "\n") {
		w.line(indent, "// "+l)
	}
}

func scalarJSON(v reflect.Value) string {
	if v.Kind() == reflect.Slice && v.IsNil() {
		return "[]"
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v.Interface()); err != nil {
		return "null"
	}
	return strings.TrimSpace(b.String())
}

// wrap breaks text into lines of at most width characters (longer words
// stay whole).
func wrap(text string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	cur := ""
	for _, word := range strings.Fields(text) {
		switch {
		case cur == "":
			cur = word
		case len(cur)+1+len(word) <= width:
			cur += " " + word
		default:
			out = append(out, cur)
			cur = word
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
