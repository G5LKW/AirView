package feed

import (
	"strings"
	"testing"
	"time"

	"airview/internal/model"
)

// Radarcape status frames (Beast type '4') as documented:
//
//   - Jetvision wiki "Mode-S Beast:Data Output Formats": <esc> "4" : 6 byte
//     MLAT timestamp, 1 byte unused, DIP switch configuration settings, time
//     stamp error ticks as int8_t (1 tick is 15ns); typical +-3 ticks.
//   - mlat-client modes_reader.c (radarcape_status_to_dict /
//     radarcape_gpsstatus_to_dict): data[0] settings (0x10 = GPS timestamps),
//     data[1] timestamp_pps_delta (int8), data[2] GPS status: 0x80
//     utc_bugfix, 0x20 timestamp NOT ok (decoder RADARCAPE_EMULATED),
//     0x10 sync_ok, 0x08 utc_offset_ok, 0x04 sats_ok, 0x02 tracking_ok,
//     0x01 antenna_ok.
//
// Each frame below is the complete wire form: 1A 34, timestamp (second of
// day 43200 = 0x2A30 << 30), unused byte, settings, error ticks, GPS status,
// then the zero padding to 14 bytes.
func rawStatusFrame(settings byte, ticks int8, gps byte) []byte {
	return AppendBeastFrame(nil, BeastStatusType, GPSTimestamp(43200, 0), 0, statusPayload(settings, ticks, gps))
}

func TestRadarcapeStatusDocumentedFrames(t *testing.T) {
	cases := []struct {
		name     string
		settings byte
		ticks    int8
		gps      byte
		verdict  string // phrase the web UI keys on
		advice   string // "" = no advice
		class    model.ClockClass
	}{
		{"locked, UTC, sync", 0x10, 1, 0x9F, "GPS locked", "", model.ClockGPSUTC},
		{"locked, GPS time base", 0x10, -1, 0x97, "GPS locked", "TimeGPS", model.ClockGPSGPST},
		// The frame behind "GPS timestamps, GPS time not valid, UTC, 1PPS
		// offset +16 ns ... clock gps_emul": bit 5 set by the Radarcape
		// itself (mlat-client: timestamp_ok false, emulated decoder). Not a
		// decoding error: the receiver says its timestamps are emulated.
		{"emulated timestamps (user report)", 0x10, 1, 0xBF, "not valid", "'Sync'", model.ClockGPSEmul},
		{"emulated, DPLL unlocked", 0x10, 1, 0xAF, "not valid", "'Sync'", model.ClockGPSEmul},
		{"fix but DPLL unlocked", 0x10, 0, 0x8F, "not synchronised", "noSync", model.ClockGPSEmul},
		{"antenna fault", 0x10, 0, 0x80, "antenna fault", "antenna", model.ClockGPSEmul},
		{"no satellites", 0x10, 0, 0x81, "no GPS satellites", "sky", model.ClockGPSEmul},
		{"acquiring", 0x10, 0, 0x83, "acquiring", "three satellites", model.ClockGPSEmul},
		{"old firmware", 0x10, 0, 0x00, "not reported", "firmware", model.ClockGPSEmul},
		{"large 1PPS error", 0x10, -9, 0x9F, "GPS locked", "-9 ticks", model.ClockGPSUTC},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []BeastStatus
			d := &BeastDecoder{Sink: &sink{}, OnStatus: func(s BeastStatus) { got = append(got, s) }}
			decodeBeast(d, time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC), rawStatusFrame(c.settings, c.ticks, c.gps))
			if len(got) != 1 {
				t.Fatalf("%d status callbacks", len(got))
			}
			st := got[0]
			if st.Settings != c.settings || st.PPSOffset != c.ticks || st.GPS != c.gps {
				t.Fatalf("bytes decoded as %02X %d %02X", st.Settings, st.PPSOffset, st.GPS)
			}
			if ns := st.PPSOffsetNs(); ns != float64(c.ticks)*15 {
				t.Errorf("1PPS error %v ns, want %d ticks x 15 ns", ns, c.ticks)
			}
			text := st.String()
			if !strings.Contains(text, c.verdict) {
				t.Errorf("text %q lacks %q", text, c.verdict)
			}
			if c.verdict != "GPS locked" && strings.Contains(text, "GPS locked") {
				t.Errorf("text %q claims a lock", text)
			}
			if a := st.Advice(); (c.advice == "") != (a == "") || !strings.Contains(a, c.advice) {
				t.Errorf("advice %q, want one containing %q", a, c.advice)
			}
			if c.advice != "" && !strings.Contains(text, " - check: ") {
				t.Errorf("text %q does not carry the advice", text)
			}
			if cls := gpsClassFromStatus(st); cls != c.class {
				t.Errorf("clock class %v, want %v", cls, c.class)
			}
			// The text and the clock class must agree: "GPS locked" only for
			// a GPS-disciplined class.
			if strings.Contains(text, "GPS locked") != (gpsClassFromStatus(st) != model.ClockGPSEmul) {
				t.Errorf("text %q disagrees with clock class %v", text, gpsClassFromStatus(st))
			}
		})
	}
}

func TestRadarcapeStatusTwelveMHz(t *testing.T) {
	st, _ := ParseBeastStatus(statusPayload(0x00, 0, 0x00))
	if s := st.String(); !strings.HasPrefix(s, "12 MHz timestamps") || st.Advice() != "" {
		t.Errorf("%q / %q", s, st.Advice())
	}
}
