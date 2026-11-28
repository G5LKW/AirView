package alerts

import (
	"strconv"
	"strings"
	"time"

	"airview/internal/model"
)

// Placeholders understood in Rule.Message:
//
//	{rule} {severity} {icao} {callsign} {reg} {type} {operator} {squawk}
//	{alt} (ft) {dist} (km) {ident} (callsign, else reg, else icao)
//	{event} {key} {value} {text} (system events) {watchlist} {note} (watch-list hit)
//	{route} {from} {to} {eta} (UTC) {routeSource} (live route) {ra} (TCAS advisory)
//
// Unknown placeholders are kept verbatim; unknown values expand to "?".
const placeholderHelp = "{rule} {severity} {icao} {callsign} {reg} {type} {operator} {squawk} {alt} {dist} {ident} {event} {key} {value} {text} {watchlist} {note} {route} {from} {to} {eta} {routeSource} {ra}"

// PlaceholderHelp lists the message template placeholders (for the UI).
func PlaceholderHelp() string { return placeholderHelp }

func (e *Engine) expand(r *Rule, al *Alert, v *view, ev *SystemEvent) string {
	tpl := r.Message
	if tpl == "" {
		tpl = defaultTemplate(al, ev)
	}
	var wlName, wlNote string
	needWL := strings.Contains(tpl, "{watchlist}") || strings.Contains(tpl, "{note}")
	if needWL && v != nil && v.icaoOK && v.watch != nil {
		wlName, wlNote, _ = v.watch.note(v.icao)
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(tpl, '{')
		if i < 0 {
			b.WriteString(tpl)
			break
		}
		j := strings.IndexByte(tpl[i:], '}')
		if j < 0 {
			b.WriteString(tpl)
			break
		}
		b.WriteString(tpl[:i])
		name := tpl[i+1 : i+j]
		val, ok := routePlaceholder(name, v)
		if !ok {
			val, ok = placeholder(name, r, al, ev, wlName, wlNote)
		}
		if ok {
			b.WriteString(val)
		} else {
			b.WriteString(tpl[i : i+j+1])
		}
		tpl = tpl[i+j+1:]
	}
	return strings.TrimSpace(b.String())
}

func q(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func placeholder(name string, r *Rule, al *Alert, ev *SystemEvent, wlName, wlNote string) (string, bool) {
	switch name {
	case "rule":
		return r.Name, true
	case "severity":
		return r.Severity, true
	case "icao":
		return q(al.ICAO), true
	case "callsign":
		return q(al.Callsign), true
	case "reg":
		return q(al.Reg), true
	case "type":
		return q(al.Type), true
	case "operator":
		return q(al.Operator), true
	case "squawk":
		return q(al.Squawk), true
	case "ident":
		switch {
		case al.Callsign != "":
			return al.Callsign, true
		case al.Reg != "":
			return al.Reg, true
		}
		return q(al.ICAO), true
	case "alt":
		if al.AltFt == nil {
			return "?", true
		}
		return strconv.FormatFloat(*al.AltFt, 'f', 0, 64), true
	case "dist":
		if al.DistKm == nil {
			return "?", true
		}
		return strconv.FormatFloat(*al.DistKm, 'f', 1, 64), true
	case "event":
		if ev == nil {
			return "?", true
		}
		return ev.Kind, true
	case "key":
		if ev == nil {
			return "?", true
		}
		return q(ev.Key), true
	case "value":
		if ev == nil || ev.Value == nil {
			return "?", true
		}
		return strconv.FormatFloat(*ev.Value, 'f', -1, 64), true
	case "text":
		if ev == nil {
			return "?", true
		}
		return q(ev.Text), true
	case "watchlist":
		return q(wlName), true
	case "note":
		return q(wlNote), true
	}
	return "", false
}

func defaultTemplate(al *Alert, ev *SystemEvent) string {
	if ev != nil {
		if ev.Text != "" {
			return "{rule}: {text}"
		}
		if ev.Key != "" {
			return "{rule}: {event} {key}"
		}
		return "{rule}: {event}"
	}
	s := "{rule}: {ident}"
	if al.Type != "" {
		s += " ({type})"
	}
	if al.ICAO != "" {
		s += " [{icao}]"
	}
	if al.Squawk != "" {
		s += " sq {squawk}"
	}
	if al.AltFt != nil {
		s += " {alt} ft"
	}
	if al.DistKm != nil {
		s += " {dist} km"
	}
	return s
}

// routePlaceholder expands the route and TCAS variables ({route}, {from},
// {to}, {eta} (UTC hh:mm), {routeSource}, {ra}) from the aircraft's live data.
func routePlaceholder(name string, v *view) (string, bool) {
	switch name {
	case "route", "from", "to", "eta", "routeSource", "ra":
	default:
		return "", false
	}
	var a *model.Aircraft
	if v != nil {
		a = v.ac
	}
	if name == "ra" {
		if a == nil || a.ACAS == nil {
			return "?", true
		}
		return a.ACAS.Advisory, true
	}
	if a == nil || a.Route == nil {
		return "?", true
	}
	r := a.Route
	code := func(icao, iata string) string {
		if iata != "" {
			return iata
		}
		return q(icao)
	}
	switch name {
	case "route":
		return code(r.From, r.FromIATA) + "-" + code(r.To, r.ToIATA), true
	case "from":
		return code(r.From, r.FromIATA), true
	case "to":
		return code(r.To, r.ToIATA), true
	case "eta":
		if r.ETA <= 0 {
			return "?", true
		}
		return time.Unix(r.ETA, 0).UTC().Format("15:04") + " UTC", true
	}
	return q(r.Source), true
}
