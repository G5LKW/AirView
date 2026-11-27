// Package hub implements the AirView "data port": an authenticated TCP
// service that relays a station's live receiver stream (raw Mode S frames
// with their original receiver timestamps and signal levels, pre-decoded
// updates, and the station's receiver/antenna settings) to remote AirView
// instances, plus the matching client.
//
// The server side is a non-blocking fan-out: Server.Broadcast and
// Server.BroadcastUpdate never wait for a client. Every client owns a bounded
// byte buffer; a client that cannot keep up is disconnected (it reconnects and
// simply resumes with live data - the stream carries no history).
//
// # Wire protocol, version 1
//
// Transport is one TCP connection per client, server default port 30154.
// The connection starts with a short text handshake (ASCII lines terminated
// by "\n", at most 512 bytes each) and then switches to binary records that
// flow from the server to the client only.
//
// ## Handshake
//
//		S -> C   AIRVIEW-HUB 1 <auth> <server-nonce>\n
//		C -> S   AUTH <client-nonce> <client-proof>\n
//		S -> C   OK <server-proof>\n            (login accepted, records follow)
//		   or    ERR <code> <free text>\n       (connection is then closed)
//
//	  - "1" is the protocol version. A client must refuse a version it does
//	    not know.
//	  - <auth> is "hmac-sha256" when the server has a password and "open" when
//	    it accepts anybody.
//	  - <server-nonce> and <client-nonce> are 32 random bytes each, written as
//	    64 lower-case hex digits.
//	  - <client-proof> = hex(HMAC-SHA256(key = password, msg = server-nonce)).
//	    The password itself never crosses the wire. In "open" mode the server
//	    ignores the proof (the client still sends one, computed with whatever
//	    password it has, possibly the empty string).
//	  - <server-proof> = hex(HMAC-SHA256(key = password, msg = client-nonce ||
//	    server-nonce)). It lets a client that has a password detect a server
//	    that does not know it. In "open" mode it is computed with the empty
//	    password and clients do not verify it.
//	  - The server compares proofs in constant time.
//	  - ERR codes: "auth" (wrong password), "busy" (client limit reached),
//	    "throttled" (too many failed logins from this IP address; retry later),
//	    "proto" (malformed or late handshake). The server may send an ERR line
//	    in place of any line it would otherwise send.
//	  - The whole handshake must finish within the server's login timeout
//	    (default 10 s).
//
// ## Admin-only mode (aggregator)
//
// On an aggregator (Options.AdminOnly / Server.SetAdminOnly) the data port
// serves only the administrator's own AirView instances:
//
//   - <auth> is always "hmac-sha256"; with no password set every login is
//     refused with "ERR auth" (open access is never offered).
//   - The AUTH line must be complete within 2 s of the connect
//     (Options.ProbeTimeout).
//   - A peer whose first byte is 0x1A (a Beast stream, e.g. the ADS-B
//     Exchange feed client's MLAT results connector), that sends no AUTH
//     line in time, or whose first line is not a well-formed AUTH line is
//     closed without an ERR line. Such closes are logged at most once per
//     IP address per hour (Options.QuietLogInterval), with a count of the
//     suppressed ones, and never count towards the failed-login throttle.
//   - Clients that send a well-formed AUTH line get the normal replies.
//
// Standalone stations do not use this mode; their behaviour is unchanged.
//
// ## Records (server -> client, after "OK")
//
// All integers are big-endian. Every record is
//
//	uint16  length   number of bytes that follow this field (type + payload), 1..65535
//	uint8   type
//	bytes   payload  (length-1 bytes)
//
// Clients must skip record types they do not know. Record types:
//
//	1  Station    payload = UTF-8 JSON of the Station struct:
//	              {"app","version","proto","receiver":model.Receiver,
//	               "antennas":[model.Antenna...],"feeds":[StationFeed...]}
//	              Sent right after login and again whenever it changes.
//
//	2  FeedDef    uint8 index, then the feed ID (UTF-8, rest of the payload).
//	              Declares the feed ID that Frame/Update records refer to by
//	              index. All known definitions are sent after login, new ones
//	              before the first record that uses them. Index 255 is
//	              reserved and means "unknown feed" (empty ID).
//
//	3  Frame      one model.RawFrame:
//	              uint8  flags    bit0 MLAT synthetic frame
//	                              bit1 GPS timestamp present
//	                              bit2 12 MHz counter present
//	                              bit3 signal level present
//	              uint8  kind     model.FrameKind (1 Mode A/C, 2 short, 3 long, 4 status)
//	              uint8  feed     feed index (see FeedDef)
//	              int8   channel  receiver RF channel, -1 = not known
//	              int64  recv     host receive time, Unix nanoseconds (0 = unknown)
//	              int64  gpsNs    only if bit1: ns since UTC midnight (RawFrame.GPSNs)
//	              uint48 clock    only if bit2: 12 MHz counter (RawFrame.Clock12MHz)
//	              float64 signal  only if bit3: dBFS, IEEE 754 bits (absent = NaN)
//	              bytes  data     rest of the payload (RawFrame.Data)
//	              The data length must fit the kind (2, 7, 14 bytes; status:
//	              1..64 bytes). Clients silently discard records where it
//	              does not, and records of a kind they do not know.
//
//	4  Update     one model.DecodedUpdate:
//	              uint16 mask     bit0 MLAT            bit7  GS present
//	                              bit1 callsign        bit8  track present
//	                              bit2 squawk          bit9  vertical rate present
//	                              bit3 lat present     bit10 on-ground present
//	                              bit4 lon present     bit11 on-ground value
//	                              bit5 baro altitude   bit12 signal present
//	                              bit6 geom altitude
//	              uint8  feed     feed index
//	              int64  recv     host receive time, Unix nanoseconds
//	              uint32 icao     address (bit 24 set = non-ICAO address)
//	              uint32 msgs     DecodedUpdate.Msgs (0 .. 2^31-1)
//	              then, in bit order and only when the bit is set:
//	              callsign (uint8 length + bytes), squawk (uint8 length + bytes),
//	              lat, lon, altBaro, altGeom, gs, track, vertRate, signal
//	              (float64 each; on-ground has no extra bytes).
//
//	5  Heartbeat  int64 host time (Unix nanoseconds), uint32 sequence number.
//	              Sent immediately after login and then every 5 s (server
//	              option). Clients treat a silent connection as dead; they
//	              also use heartbeats to translate host receive times into
//	              their own clock.
//
// The client sends nothing after its AUTH line; anything it does send is
// read and discarded by the server (reading is how the server notices a
// closed connection quickly).
//
// ## Delivery semantics
//
// Records are delivered in the order the host produced them. There is no
// acknowledgement, replay or back-pressure. When a client's server-side
// buffer (default 4 MiB) overflows, or a write does not complete within the
// write timeout, the server closes the connection.
package hub
