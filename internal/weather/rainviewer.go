package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// RainViewer tile parameters (https://www.rainviewer.com/api/weather-maps-api.html).
const (
	// RadarMaxZoom is the highest zoom level RainViewer serves; set it as the
	// raster source's maxzoom and let the map over-zoom beyond it.
	RadarMaxZoom = 7
	// RadarTileSize is the tile size used in the ready-made URL templates.
	RadarTileSize = 256
	// RadarColorScheme is the colour scheme in the ready-made templates
	// (2 = "Universal Blue"; the public API renders every non-zero scheme
	// with it; 0 = raw dBZ greyscale).
	RadarColorScheme = 2
)

// RadarFrame is one radar mosaic in time.
type RadarFrame struct {
	T    int64  `json:"t"`    // Unix ms the frame was generated
	Path string `json:"path"` // frame path on the tile host, e.g. "/v2/radar/1a2b3c4d5e6f"
	// URL is a ready-to-use XYZ raster tile template with {z}/{x}/{y}
	// placeholders: 256 px, Universal Blue, smoothed, snow shown separately.
	URL string `json:"url"`
}

// Radar is the RainViewer frame index for a precipitation-radar map overlay.
type Radar struct {
	Fetched        int64        `json:"fetched"`   // Unix ms of the download
	Generated      int64        `json:"generated"` // Unix ms the index was generated upstream
	Host           string       `json:"host"`      // tile host, e.g. "https://tilecache.rainviewer.com"
	Past           []RadarFrame `json:"past"`      // observed frames, oldest first (about 2 h in 10-minute steps)
	Nowcast        []RadarFrame `json:"nowcast"`   // forecast frames (currently empty on the free API)
	TileSize       int          `json:"tileSize"`
	MaxZoom        int          `json:"maxZoom"`
	Attribution    string       `json:"attribution"`
	AttributionURL string       `json:"attributionUrl"`
}

// Latest returns the newest observed frame.
func (r *Radar) Latest() (RadarFrame, bool) {
	if r == nil || len(r.Past) == 0 {
		return RadarFrame{}, false
	}
	return r.Past[len(r.Past)-1], true
}

// TileURL builds an XYZ tile template for a frame with explicit rendering
// options: size 256 or 512, colour scheme, smoothing and separate snow colours.
func (r *Radar) TileURL(f RadarFrame, size, color int, smooth, snow bool) string {
	return radarTileURL(r.Host, f.Path, size, color, smooth, snow)
}

func radarTileURL(host, path string, size, color int, smooth, snow bool) string {
	if size != 512 {
		size = 256
	}
	if color < 0 || color > 255 {
		color = RadarColorScheme
	}
	b2i := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	return host + path + "/" + strconv.Itoa(size) + "/{z}/{x}/{y}/" + strconv.Itoa(color) + "/" + b2i(smooth) + "_" + b2i(snow) + ".png"
}

var radarPathRE = regexp.MustCompile(`^/[A-Za-z0-9/_.\-]{1,200}$`)

// RadarFrames downloads the RainViewer weather-maps index. The frame list
// changes every 10 minutes; poll no more often than every few minutes.
func (c *Client) RadarFrames(ctx context.Context) (*Radar, error) {
	body, _, err := c.get(ctx, request{url: c.rainViewerURL})
	if err != nil {
		return nil, err
	}
	r, err := parseRadar(body)
	if err != nil {
		return nil, err
	}
	r.Fetched = c.now().UnixMilli()
	return r, nil
}

// parseRadar decodes and sanitises a weather-maps.json document. The host
// and paths end up in tile URLs handed to the browser, so they are validated
// rather than trusted.
func parseRadar(body []byte) (*Radar, error) {
	type frame struct {
		Time int64  `json:"time"`
		Path string `json:"path"`
	}
	var doc struct {
		Version   string `json:"version"`
		Generated int64  `json:"generated"`
		Host      string `json:"host"`
		Radar     struct {
			Past    []frame `json:"past"`
			Nowcast []frame `json:"nowcast"`
		} `json:"radar"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("weather: decoding RainViewer index: %w", err)
	}
	u, err := url.Parse(doc.Host)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("weather: RainViewer index has an unusable tile host %q", truncate(doc.Host, 80))
	}
	hn := strings.ToLower(u.Hostname())
	if hn != "rainviewer.com" && !strings.HasSuffix(hn, ".rainviewer.com") {
		return nil, fmt.Errorf("weather: RainViewer index points at a foreign tile host %q", truncate(doc.Host, 80))
	}
	host := "https://" + u.Host

	conv := func(in []frame) []RadarFrame {
		out := make([]RadarFrame, 0, len(in))
		for _, f := range in {
			if f.Time <= 0 || !radarPathRE.MatchString(f.Path) || strings.Contains(f.Path, "..") {
				continue
			}
			out = append(out, RadarFrame{
				T:    f.Time * 1000,
				Path: f.Path,
				URL:  radarTileURL(host, f.Path, RadarTileSize, RadarColorScheme, true, true),
			})
		}
		return out
	}
	r := &Radar{
		Generated:      doc.Generated * 1000,
		Host:           host,
		Past:           conv(doc.Radar.Past),
		Nowcast:        conv(doc.Radar.Nowcast),
		TileSize:       RadarTileSize,
		MaxZoom:        RadarMaxZoom,
		Attribution:    RadarAttribution,
		AttributionURL: RadarAttributionURL,
	}
	if len(r.Past) == 0 && len(r.Nowcast) == 0 {
		return nil, errors.New("weather: RainViewer index contains no radar frames")
	}
	return r, nil
}
