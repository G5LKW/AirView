package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"airview/internal/model"
)

// Setup flags of the phase 4-5 features (WP-WIRE-45): Beast outputs to
// other aggregators and the RTL-SDR input through the user's own rtl_tcp.
// Every value they write is an ordinary feeder.json setting (also settable
// with "airview-feeder config set" and AIRVIEW_FEEDER_* variables).

func registerExtraSetupFlags(fs *flag.FlagSet, o *setupOptions) {
	fs.StringVar(&o.beastOut, "beast-out", "", "also feed other aggregators: Beast output(s) to `host[:port],...` (default port 30004; \"off\" removes all)")
	fs.StringVar(&o.beastFormat, "beast-format", "", "Beast output `format`: beast_reduce_plus_out (default), beast_reduce_out or beast_out")
	fs.StringVar(&o.beastUUID, "beast-uuid", "", "station `UUID` sent to the Beast aggregator(s) (default: a random one per output)")
	fs.StringVar(&o.sdrGain, "sdr-gain", "", "RTL-SDR gain: `max` (default), auto or dB such as 40.2 (input type rtltcp)")
	fs.IntVar(&o.sdrPPM, "sdr-ppm", 0, "RTL-SDR oscillator correction in `ppm` (-500..500)")
	fs.IntVar(&o.sdrSampleRate, "sdr-sample-rate", 0, "RTL-SDR sample `rate` in samples/s (2000000..3200000, default 2400000)")
	fs.IntVar(&o.sdrDevice, "sdr-device", 0, "dongle `index` for a spawned rtl_tcp (0..31)")
	fs.BoolVar(&o.sdrBiasTee, "sdr-bias-tee", false, "switch on the dongle's antenna supply (bias tee)")
	fs.BoolVar(&o.sdrSpawn, "sdr-spawn", false, "start your own rtl_tcp as a child process (never shipped or downloaded by AirView)")
	fs.StringVar(&o.sdrRTLTCPPath, "sdr-rtltcp-path", "", "rtl_tcp `executable` for -sdr-spawn (default: search PATH and the program folder)")
	fs.BoolVar(&o.sdrAcceptUnverAP, "sdr-accept-unverified-ap", false, "pass address/parity replies of unconfirmed addresses (not recommended)")
}

// sdrFlagNames are the flags that change input.sdr.
var sdrFlagNames = []string{"sdr-gain", "sdr-ppm", "sdr-sample-rate", "sdr-device", "sdr-bias-tee", "sdr-spawn", "sdr-rtltcp-path", "sdr-accept-unverified-ap"}

// applySDRFlags writes the -sdr-* flags into the receiver's input.sdr. They
// need an rtltcp input.
func applySDRFlags(r *model.FeederReceiver, o setupOptions) error {
	var given []string
	for _, n := range sdrFlagNames {
		if o.given[n] {
			given = append(given, "-"+n)
		}
	}
	if len(given) == 0 {
		return nil
	}
	if r.Input.Type != model.FeedRTLTCP {
		return fmt.Errorf("%s need an RTL-SDR input: add -input 127.0.0.1:1234 -input-type rtltcp", strings.Join(given, ", "))
	}
	s := &r.Input.SDR
	if o.given["sdr-gain"] {
		s.Gain = o.sdrGain
	}
	if o.given["sdr-ppm"] {
		s.PPM = o.sdrPPM
	}
	if o.given["sdr-sample-rate"] {
		s.SampleRate = o.sdrSampleRate
	}
	if o.given["sdr-device"] {
		s.Device = o.sdrDevice
	}
	if o.given["sdr-bias-tee"] {
		s.BiasTee = o.sdrBiasTee
	}
	if o.given["sdr-spawn"] {
		s.Spawn = o.sdrSpawn
	}
	if o.given["sdr-rtltcp-path"] {
		s.RTLTCPPath = o.sdrRTLTCPPath
	}
	if o.given["sdr-accept-unverified-ap"] {
		s.AcceptUnverifiedAP = o.sdrAcceptUnverAP
	}
	if notes := s.Normalize("input.sdr"); len(notes) > 0 {
		return errors.New(strings.Join(notes, "; "))
	}
	return nil
}

// applyBeastOutFlags adds, updates or removes Beast outputs.
func applyBeastOutFlags(cfg *model.FeederConfig, o setupOptions) error {
	v := strings.TrimSpace(o.beastOut)
	if v == "" {
		if o.given["beast-format"] || o.given["beast-uuid"] {
			n := 0 // apply to the existing Beast outputs
			for i := range cfg.Uplinks {
				if cfg.Uplinks[i].IsBeast() {
					setBeastOpts(&cfg.Uplinks[i], o)
					n++
				}
			}
			if n == 0 {
				return errors.New("-beast-format and -beast-uuid need a Beast output (-beast-out host:port)")
			}
		}
		return nil
	}
	if strings.EqualFold(v, "off") || strings.EqualFold(v, "none") {
		out := cfg.Uplinks[:0]
		for _, u := range cfg.Uplinks {
			if !u.IsBeast() {
				out = append(out, u)
			}
		}
		cfg.Uplinks = out
		return nil
	}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		h, port, err := splitHostPortDefault(part, model.DefaultBeastOutPort)
		if err != nil || h == "" {
			return fmt.Errorf("-beast-out %q: want host[:port]", part)
		}
		idx := -1
		for i, u := range cfg.Uplinks {
			if u.IsBeast() && strings.EqualFold(u.Host, h) && u.Port == port {
				idx = i
			}
		}
		if idx < 0 {
			u := model.DefaultUplink()
			u.Type, u.Host, u.Port = model.UplinkBeast, h, port
			u.ID = uniqueUplinkID(cfg.Uplinks, "beast-"+slugFromHost(h))
			cfg.Uplinks = append(cfg.Uplinks, u)
			idx = len(cfg.Uplinks) - 1
		}
		setBeastOpts(&cfg.Uplinks[idx], o)
	}
	return nil
}

func setBeastOpts(u *model.Uplink, o setupOptions) {
	if o.given["beast-format"] {
		u.Format = strings.TrimSpace(o.beastFormat)
	}
	if o.given["beast-uuid"] {
		u.UUID = strings.TrimSpace(o.beastUUID)
	}
	switch {
	case o.noTimestamps:
		u.Timestamps = false
	case o.timestamps:
		u.Timestamps = true
	}
}

// uniqueUplinkID returns base, or base-2, base-3, ... when taken.
func uniqueUplinkID(ups []model.Uplink, base string) string {
	taken := map[string]bool{}
	for _, u := range ups {
		taken[u.ID] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		id := fmt.Sprintf("%s-%d", base, i)
		if !taken[id] {
			return id
		}
	}
}
