package engine

import (
	"math"
	"testing"

	"airview/internal/model"
)

func TestStationOf(t *testing.T) {
	cfg := model.DefaultConfig()
	gps := &model.ReceiverGPS{FeedID: "rc", Lat: 50.1, Lon: 8.6, AltM: 140}

	// Not located: the receiver's GPS position, else none.
	cfg.Receiver.Located = false
	if s := stationOf(cfg, gps); s.Source != "gps" || s.Lat != 50.1 || s.Lon != 8.6 || s.HeightM != 140 {
		t.Errorf("gps fallback: %+v", s)
	}
	if s := stationOf(cfg, nil); s.Source != "none" || !math.IsNaN(s.Lat) {
		t.Errorf("none: %+v", s)
	}
	if s := stationOf(cfg, &model.ReceiverGPS{}); s.Source != "none" {
		t.Errorf("0,0 GPS position must be ignored: %+v", s)
	}

	// Located: the configuration wins; height = ground + highest antenna.
	cfg.Receiver.Located, cfg.Receiver.Lat, cfg.Receiver.Lon, cfg.Receiver.GroundElevM = true, 48, 11, 500
	cfg.Antennas = []model.Antenna{{ID: "a", HeightAGLM: 6}, {ID: "b", HeightAGLM: 12}}
	if s := stationOf(cfg, gps); s.Source != "config" || s.Lat != 48 || s.HeightM != 512 {
		t.Errorf("config: %+v", s)
	}
}
