# internal/feederui — wiring notes (WP-FB)

Local status page of airview-feeder (DESIGN §12.1). Consumers: `cmd/airview-feeder` (all
platforms), the Windows service/tray (WP-WF).

```go
st := feederui.NewStats(nil)                  // model.FrameSink: put it in the feed tee
h := feederui.New(feederui.Options{
    Backend:      backend,                    // implements feederui.Backend (status, claim, retry, settings)
    PasswordHash: func() string { return cfg.UI.PasswordHash },
    Logf:         log.Printf,                 // never receives secrets
})
http.Serve(ln, h)                             // default 127.0.0.1:8099
```

API: `GET /api/status` (Snapshot), `GET /api/health` (200 all uplinks connected / 503),
`POST /api/claim {"id","rxNo"}` -> `{"code","expiresMs"}`, `POST /api/retry {"id"}`,
`GET /api/settings` (SettingsDoc, secrets = `Redacted`), `PUT /api/settings` (FeederConfig;
`Redacted` keeps a stored secret). Mutations need `X-AirView-Feeder: 1`; Host must be an IP
literal or localhost; non-loopback clients need the password (Basic auth).

`Stats.HandleFrame` ~33 ns/frame, 0 allocs (i9-13900K). It ignores status frames and
returned MLAT results; addresses count only when parity proves them (DF11/17/18).
