package navdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PhotoOptions configure the optional aircraft photo metadata lookup at
// planespotters.net (public photo API, GET <BaseURL>/hex/<HEX>[?reg=<REG>]).
//
// Verified November 2026: the API answers server-side requests only when the
// User-Agent contains a contact URL or e-mail ("Server User-Agent strings must
// include a contact URL or email so we can reach you, e.g.
// MyFlightTracker/1.2 (+https://example.com/contact)"); anything else gets
// HTTP 403. The lookup therefore stays disabled until the user enters a
// contact. Response fields used: photos[0].thumbnail.{src,size},
// photos[0].thumbnail_large.{src,size}, photos[0].link,
// photos[0].photographer (the same fields tar1090 uses). The UI must always
// show the photographer credit and link to the photo page with the image
// (Photo.Attribution / Photo.Link); Photo is only Found when both exist.
type PhotoOptions struct {
	Enabled     bool          `json:"enabled"`               // default false
	Contact     string        `json:"contact"`               // contact URL or e-mail for the User-Agent (required)
	BaseURL     string        `json:"baseUrl,omitempty"`     // default "https://api.planespotters.net/pub/photos"
	MinInterval time.Duration `json:"minInterval,omitempty"` // default 1 s
	DailyLimit  int           `json:"dailyLimit,omitempty"`  // default 2000
	CacheTTL    time.Duration `json:"cacheTtl,omitempty"`    // default 7 days
	NegativeTTL time.Duration `json:"negativeTtl,omitempty"` // default 2 days
	MaxWait     time.Duration `json:"maxWait,omitempty"`     // default 5 s
}

// PlanespottersURL is the default photo API base.
const PlanespottersURL = "https://api.planespotters.net/pub/photos"

func (o PhotoOptions) withDefaults() PhotoOptions {
	if o.BaseURL == "" {
		o.BaseURL = PlanespottersURL
	}
	if o.MinInterval <= 0 {
		o.MinInterval = time.Second
	}
	if o.DailyLimit <= 0 {
		o.DailyLimit = 2000
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = 7 * 24 * time.Hour
	}
	if o.NegativeTTL <= 0 {
		o.NegativeTTL = 2 * 24 * time.Hour
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 5 * time.Second
	}
	o.Contact = strings.TrimSpace(o.Contact)
	return o
}

// validContact accepts an http(s) URL or something that looks like an e-mail.
func validContact(c string) bool {
	if c == "" || len(c) > 200 || strings.ContainsAny(c, "()\r\n\t") {
		return false
	}
	if u, err := url.Parse(c); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		return true
	}
	at := strings.IndexByte(c, '@')
	return at > 0 && at < len(c)-3 && strings.Contains(c[at:], ".") && !strings.ContainsAny(c, " /")
}

// PhotoImage is one image variant.
type PhotoImage struct {
	Src    string `json:"src"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// Photo is aircraft photo metadata. Display only with Attribution and Link.
type Photo struct {
	ICAO         string      `json:"icao"`
	Found        bool        `json:"found"`
	Thumbnail    *PhotoImage `json:"thumbnail,omitempty"`
	Large        *PhotoImage `json:"large,omitempty"`
	Link         string      `json:"link,omitempty"`
	Photographer string      `json:"photographer,omitempty"`
	Attribution  string      `json:"attribution,omitempty"` // "© <photographer> / planespotters.net"
	Source       string      `json:"source,omitempty"`      // "planespotters.net"
	Cached       bool        `json:"cached,omitempty"`
	Fetched      int64       `json:"fetched,omitempty"` // Unix ms
}

type psResponse struct {
	Photos []struct {
		Thumbnail      psImage `json:"thumbnail"`
		ThumbnailLarge psImage `json:"thumbnail_large"`
		Link           string  `json:"link"`
		Photographer   string  `json:"photographer"`
	} `json:"photos"`
}

type psImage struct {
	Src  string `json:"src"`
	Size struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"size"`
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && len(s) < 1024
}

func hex6(icao uint32) string { return fmt.Sprintf("%06X", icao&0xFFFFFF) }

// Photo returns photo metadata for an airframe (cached; online only when
// enabled). reg (optional) helps the API choose the right airframe.
func (n *NavData) Photo(ctx context.Context, icao uint32, reg string) (Photo, error) {
	if icao == 0 || icao > 0xFFFFFF {
		return Photo{}, fmt.Errorf("%w: icao", ErrBadInput)
	}
	if n.st == nil {
		return Photo{}, ErrNoStore
	}
	p := Photo{ICAO: hex6(icao)}
	if cp, ok, err := n.cachedPhoto(ctx, icao); err != nil {
		return p, err
	} else if ok {
		return cp, nil
	}
	o := n.options().Photos
	if !o.Enabled {
		return p, ErrDisabled
	}
	if !validContact(o.Contact) {
		return p, ErrContactMissing
	}
	v, err := n.flight.do("photo:"+p.ICAO, func() (any, error) { return n.fetchPhoto(ctx, icao, reg, o) })
	if err != nil {
		return p, err
	}
	return v.(Photo), nil
}

func (n *NavData) cachedPhoto(ctx context.Context, icao uint32) (Photo, bool, error) {
	if pp, ok := n.pend.photo(icao, time.Now()); ok {
		return pp, true, nil // queued, not committed yet (pending.go)
	}
	p := Photo{ICAO: hex6(icao)}
	var found int
	var fetched int64
	var t, l PhotoImage
	err := n.st.DB().QueryRowContext(ctx, `SELECT fetched, found, thumb, thumb_w, thumb_h, large, large_w, large_h, link, photographer
  FROM navdata_photo WHERE icao = ? AND expires > ?`, int64(icao), time.Now().Unix()).
		Scan(&fetched, &found, &t.Src, &t.Width, &t.Height, &l.Src, &l.Width, &l.Height, &p.Link, &p.Photographer)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	p.Cached, p.Fetched = true, fetched*1000
	if found != 0 {
		finishPhoto(&p, t, l)
	}
	return p, true, nil
}

// finishPhoto sets the image fields and Found only when attribution is complete.
func finishPhoto(p *Photo, t, l PhotoImage) {
	if !httpsURL(t.Src) || !httpsURL(p.Link) || strings.TrimSpace(p.Photographer) == "" {
		p.Found, p.Thumbnail, p.Large, p.Link, p.Photographer = false, nil, nil, "", ""
		return
	}
	p.Found = true
	p.Thumbnail = &t
	if httpsURL(l.Src) {
		p.Large = &l
	}
	p.Source = "planespotters.net"
	p.Attribution = "© " + p.Photographer + " / planespotters.net"
}

func (n *NavData) fetchPhoto(ctx context.Context, icao uint32, reg string, o PhotoOptions) (Photo, error) {
	p := Photo{ICAO: hex6(icao)}
	wait, err := n.photo.reserve(o.MaxWait)
	if err != nil {
		return p, err
	}
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return p, ctx.Err()
		case <-t.C:
		}
	}
	opts := n.options()
	u := strings.TrimRight(o.BaseURL, "/") + "/hex/" + p.ICAO
	if reg = strings.ToUpper(strings.TrimSpace(reg)); reg != "" && len(reg) <= 12 && !strings.ContainsAny(reg, "/?#&% ") {
		u += "?reg=" + url.QueryEscape(reg)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return p, err
	}
	req.Header.Set("User-Agent", opts.UserAgent+" (+"+o.Contact+")")
	req.Header.Set("Accept", "application/json")
	resp, err := opts.Client.Do(req)
	if err != nil {
		n.photo.backoff(30 * time.Second)
		return p, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return p, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		n.photo.backoff(retryAfter(resp.Header.Get("Retry-After"), 10*time.Minute))
		return p, ErrRateLimited
	case http.StatusForbidden:
		n.photo.backoff(10 * time.Minute)
		return p, fmt.Errorf("navdata: planespotters.net refused the request (HTTP 403): %s", strings.TrimSpace(string(body[:min(len(body), 300)])))
	default:
		n.photo.backoff(time.Minute)
		return p, fmt.Errorf("navdata: planespotters.net: HTTP %s", resp.Status)
	}
	var r psResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return p, fmt.Errorf("navdata: planespotters.net: %w", err)
	}
	now := time.Now()
	var t, l PhotoImage
	if len(r.Photos) > 0 {
		ph := r.Photos[0]
		t = PhotoImage{Src: ph.Thumbnail.Src, Width: ph.Thumbnail.Size.Width, Height: ph.Thumbnail.Size.Height}
		l = PhotoImage{Src: ph.ThumbnailLarge.Src, Width: ph.ThumbnailLarge.Size.Width, Height: ph.ThumbnailLarge.Size.Height}
		p.Link, p.Photographer = strings.TrimSpace(ph.Link), strings.TrimSpace(ph.Photographer)
		if len(p.Photographer) > 200 {
			p.Photographer = p.Photographer[:200]
		}
	}
	finishPhoto(&p, t, l)
	p.Fetched = now.UnixMilli()
	exp := now.Add(o.NegativeTTL)
	if p.Found {
		exp = now.Add(o.CacheTTL)
	} else {
		t, l = PhotoImage{}, PhotoImage{}
	}
	// The row rides the store's group commit; until it is visible the
	// overlay answers with what cachedPhoto will read back from it.
	cached := Photo{ICAO: p.ICAO, Link: p.Link, Photographer: p.Photographer, Cached: true, Fetched: now.Unix() * 1000}
	if p.Found {
		finishPhoto(&cached, t, l)
	}
	n.pend.putPhoto(icao, cached, exp.Unix(), now)
	found, link, photographer := b2i(p.Found), p.Link, p.Photographer
	n.queueWrite("navdata_photo", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM navdata_photo WHERE expires <= ?`, now.Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO navdata_photo(icao, fetched, expires, found, thumb, thumb_w, thumb_h,
  large, large_w, large_h, link, photographer) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, int64(icao), now.Unix(), exp.Unix(),
			found, t.Src, t.Width, t.Height, l.Src, l.Width, l.Height, link, photographer)
		return err
	})
	return p, nil
}
