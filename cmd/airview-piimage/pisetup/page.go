package main

// consentTimestamps is text C1 of docs/aggregator/CONSENT.md, word for word.
const consentTimestamps = "If your receiver sends timestamps, the network operator can determine its location precisely, whatever you enter. Other users and the public cannot. Choose 'No timestamps (no MLAT)' if you do not want this."

// consentPinLines is text C2 of docs/aggregator/CONSENT.md, word for word
// (one element per paragraph or list item).
var consentPinLines = []string{
	"Show my station on the public map?",
	"Hidden (default): no pin.",
	"Region: a point placed at random inside a 0.5-degree area around your station, about 55 by 35 km in the UK.",
	"Area: the same inside a 0.1-degree area, about 11 by 7 km. Where fewer than 3 stations are nearby it is shown as Region.",
	"The point is chosen once and stays put, so it cannot be averaged out. Your exact position is never public. You can change this at any time.",
}

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>AirView Feeder setup</title>
<style>
:root{--bg:#0b1526;--panel:#12223a;--line:#22395c;--text:#e6edf7;--muted:#9fb3cf;--accent:#3d8bfd;--err:#ff6b6b;--ok:#3ecf8e}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:760px;margin:0 auto;padding:24px 16px 64px}
h1{font-size:24px;margin:0 0 4px}
h2{font-size:17px;margin:28px 0 8px;color:var(--accent)}
p.lead{color:var(--muted);margin:0 0 16px}
section{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:16px;margin:12px 0}
label{display:block;margin:12px 0 4px;font-weight:600}
.hint{color:var(--muted);font-size:13px;margin:2px 0 0}
input[type=text],input[type=password],select{width:100%;padding:9px 10px;border-radius:7px;border:1px solid var(--line);background:#0e1b2f;color:var(--text);font:inherit}
input:focus,select:focus{outline:2px solid var(--accent);outline-offset:1px}
.row{display:grid;grid-template-columns:1fr 1fr;gap:12px}
@media (max-width:560px){.row{grid-template-columns:1fr}}
.choice{display:flex;gap:8px;align-items:flex-start;margin:6px 0;font-weight:400}
.choice input{margin-top:4px}
.consent{border-left:3px solid var(--accent);padding:8px 12px;background:#0e1b2f;border-radius:6px;font-size:14px}
.consent p{margin:4px 0}
.error{border:1px solid var(--err);color:var(--err);border-radius:8px;padding:10px 12px;margin:12px 0}
.bad{border-color:var(--err)!important}
.ok{border:1px solid var(--ok);border-radius:8px;padding:12px;margin:12px 0}
button{margin-top:20px;padding:11px 22px;border:0;border-radius:8px;background:var(--accent);color:#fff;font:inherit;font-weight:600;cursor:pointer}
button[disabled]{opacity:.6;cursor:wait}
pre{white-space:pre-wrap;background:#0e1b2f;border:1px solid var(--line);border-radius:6px;padding:8px;font-size:12px;max-height:320px;overflow:auto}
a{color:var(--accent)}
code{background:#0e1b2f;padding:1px 4px;border-radius:4px}
</style>
</head>
<body>
<main>
<h1>AirView Feeder setup</h1>
{{if .Done}}
<div class="ok">
<p><strong>Done.</strong> The feeder is installed and starting. This setup page has closed.</p>
<p>The feeder's status page is on port 8099 of this Pi; sign in with any user name and the password you chose:</p>
<ul>{{range .Addrs}}<li><a href="http://{{.}}:8099/">http://{{.}}:8099/</a></li>{{end}}</ul>
<p class="hint">The status page accepts IP addresses (not the .local name) to protect against DNS rebinding. Next: claim the station on the aggregator's web site with the claim code from the status page.</p>
</div>
{{else}}
<p class="lead">This Raspberry Pi ({{.Host}}) runs the AirView Network feeder. Fill in the form once; the page disappears when the feeder is set up. Values from <code>airview/feeder-setup.txt</code> on the SD card are filled in already.</p>
{{if .Err}}<div class="error" role="alert">{{.Err}}</div>{{end}}
{{if .Log}}<details open><summary>Installer output</summary><pre>{{.Log}}</pre></details>{{end}}
<form method="post" action="/setup" id="f" autocomplete="off">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<section>
<h2>Aggregator</h2>
<label for="server">Aggregator address</label>
<input type="text" id="server" name="server" value="{{.V.Server}}" placeholder="feed.example.net:30160" required {{if eq .Field "server"}}class="bad" autofocus{{end}}>
<p class="hint">From the network's operator or its "/feeder" page. The port is 30160 unless stated otherwise.</p>
<label for="pin">Server key (optional)</label>
<input type="text" id="pin" name="pin" value="{{.V.Pin}}" placeholder="sha256/..." {{if eq .Field "pin"}}class="bad" autofocus{{end}}>
<p class="hint">Copy it from the aggregator's install command when its certificate is self-signed. Empty: the server must have a public certificate.</p>
<label for="token">Enrol token</label>
<input type="text" id="token" name="token" value="{{.V.Token}}"{{if .TokenKept}} placeholder="from feeder-setup.txt (kept, not shown)"{{end}} {{if eq .Field "token"}}class="bad" autofocus{{end}}>
<p class="hint">Needed when the network admits feeders by invitation.{{if .TokenKept}} feeder-setup.txt provided one: leave the field empty to use it.{{end}}</p>
</section>
<section>
<h2>Station</h2>
<label for="name">Station name</label>
<input type="text" id="name" name="name" value="{{.V.Name}}" maxlength="32" pattern="[A-Za-z0-9_\-]{1,32}" required {{if eq .Field "name"}}class="bad" autofocus{{end}}>
<label for="input">Receiver</label>
<input type="text" id="input" name="input" value="{{.V.Input}}" {{if eq .Field "input"}}class="bad" autofocus{{end}}>
<p class="hint"><code>auto</code> finds a decoder on this Pi (Beast output on port 30005, e.g. readsb) or a Radarcape. Otherwise host:port of a Beast output. The feeder never installs a decoder for you.</p>
</section>
<section>
<h2>Antenna position</h2>
<div class="consent"><p>{{.C1}}</p></div>
<label class="choice"><input type="radio" name="location" value="manual" {{if not .V.NoLocation}}checked{{end}}> I enter the position of the antenna</label>
<label class="choice"><input type="radio" name="location" value="network" {{if .V.NoLocation}}checked{{end}}> Let the network locate me (sends no position)</label>
<div class="row">
<div><label for="lat">Latitude</label><input type="text" id="lat" name="lat" value="{{.V.Lat}}" placeholder="51.4700" inputmode="decimal" {{if eq .Field "lat"}}class="bad" autofocus{{end}}></div>
<div><label for="lon">Longitude</label><input type="text" id="lon" name="lon" value="{{.V.Lon}}" placeholder="-0.4543" inputmode="decimal" {{if eq .Field "lon"}}class="bad" autofocus{{end}}></div>
</div>
<div class="row">
<div><label for="alt">Antenna altitude (with unit)</label><input type="text" id="alt" name="alt" value="{{.V.Alt}}" placeholder="35m or 115ft" {{if eq .Field "alt"}}class="bad" autofocus{{end}}></div>
<div><label for="altRef">Altitude reference</label><select id="altRef" name="altRef"><option value="msl" {{if ne .V.AltRef "hae"}}selected{{end}}>Mean sea level (maps)</option><option value="hae" {{if eq .V.AltRef "hae"}}selected{{end}}>GPS ellipsoid height</option></select></div>
</div>
<label for="agl">Antenna height above ground (with unit)</label>
<input type="text" id="agl" name="agl" value="{{.V.AGL}}" placeholder="10m" {{if eq .Field "agl"}}class="bad" autofocus{{end}}>
<label>Timestamps</label>
<label class="choice"><input type="radio" name="timestamps" value="yes" {{if .V.Timestamps}}checked{{end}}> Send timestamps (MLAT)</label>
<label class="choice"><input type="radio" name="timestamps" value="no" {{if not .V.Timestamps}}checked{{end}}> No timestamps (no MLAT)</label>
</section>
<section>
<h2>Public map</h2>
<div class="consent">{{range $i, $l := .C2}}<p>{{if eq $i 0}}<strong>{{$l}}</strong>{{else}}{{$l}}{{end}}</p>{{end}}</div>
<label class="choice"><input type="radio" name="privacy" value="hidden" {{if or (eq .V.Privacy "hidden") (eq .V.Privacy "")}}checked{{end}}> Hidden</label>
<label class="choice"><input type="radio" name="privacy" value="region" {{if eq .V.Privacy "region"}}checked{{end}}> Region</label>
<label class="choice"><input type="radio" name="privacy" value="cell01" {{if eq .V.Privacy "cell01"}}checked{{end}}> Area</label>
</section>
<section>
<h2>Status page and updates</h2>
<label for="password">Status page password</label>
<input type="password" id="password" name="password" minlength="8" required {{if eq .Field "password"}}class="bad" autofocus{{end}}>
<label for="password2">Repeat the password</label>
<input type="password" id="password2" name="password2" minlength="8" required {{if eq .Field "password2"}}class="bad" autofocus{{end}}>
<p class="hint">Protects the feeder's status page on your network (port 8099). It is stored only as a hash.</p>
{{if .V.CanAutoUpdate}}<label class="choice"><input type="checkbox" name="autoUpdate" value="yes" {{if .V.AutoUpdate}}checked{{end}}> Install feeder updates automatically (daily check, signed releases only)</label>
{{else}}<label class="choice"><input type="checkbox" name="autoUpdate" value="yes" disabled> Install feeder updates automatically (not available)</label>
<p class="hint" id="noupdate">This image carries no release signing key, so the feeder cannot verify updates and will not update itself. Update it later with <code>sudo airview-feeder-setup --update --manifest-key KEY</code>, or put MANIFEST_KEY= in feeder-setup.txt.</p>{{end}}
</section>
<button type="submit" id="go">Install and start the feeder</button>
</form>
<script>
document.getElementById('f').addEventListener('submit',function(){var b=document.getElementById('go');b.disabled=true;b.textContent='Installing... (up to a minute)';});
</script>
{{end}}
</main>
</body>
</html>
`
