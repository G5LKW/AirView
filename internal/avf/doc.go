// Package avf implements AVF/1, the AirView Feed protocol that native
// feeders (cmd/airview-feeder, an AirView station's uplink) use to send
// receiver data to an AirView aggregator over TCP port 30160.
//
// The normative wire specification is docs/aggregator/AVF.md (derived from
// docs/aggregator/DESIGN.md §4). This package is the single implementation
// of that wire format; internal/uplink (client) and internal/ingest/avfsrv
// (server) build their sessions on it. AVF/1 is AirView's own protocol and
// this code is written from that specification only.
//
// # Layers
//
//   - TLS (tls.go): TLS 1.3 only, ALPN "avf/1". The server presents either
//     an operator certificate (CertReloader, reloaded on mtime change) or a
//     self-signed ECDSA P-256 certificate (LoadOrCreateSelfSigned). Clients
//     verify by WebPKI or by SPKI pin ("sha256/<b64>", ClientTLSConfig).
//   - Handshake (handshake.go): three length-prefixed JSON messages,
//     HELLO_S, HELLO_C and WELCOME. The feeder proves its Ed25519 identity
//     with a signature over the server nonce and the TLS exporter value, so a
//     relay that terminates TLS (a man in the middle) cannot reuse it.
//     ServerHandshake / ClientHandshake run the two sides.
//   - Stream (stream.go): after WELCOME each direction is one raw DEFLATE
//     stream (level 1) with sync flushes at batchMs, at 32 KiB or after a
//     control record. The reading side enforces decompression guards
//     (output rate and inflate ratio).
//   - Records (record.go, payload.go, json.go): "u8 type | uvarint len |
//     payload" framing, the record type registry and the codecs of every
//     binary and JSON payload, with the byte-exact test vectors of AVF.md.
//   - Session (conn.go): Conn ties the above together after the handshake:
//     direction checks, idle/write deadlines, automatic PONG replies on the
//     client, PING/ACK keepalive and RTT/host-offset estimation on the
//     server.
//   - Sequencing (seq.go): seqEpoch generation, the server watermark and its
//     store (duplicate drop, gap detection, 10-minute retention) and a client
//     resend ring with the resume rule of AVF.md §5.2.
//   - Flow (flow.go): client FLOW state (pause, batch, slow factor), a token
//     bucket for the 25 % backfill share, the server's FLOW/selective/limit
//     decision helper, and the reconnect backoff of AVF.md §9.1.
//   - Identity (identity.go): the feeder's Ed25519 key file.
//
// All decoders are defensive: they never panic on hostile input, bound every
// length before allocating and report malformed input as an error wrapping
// ErrProtocol. Decoders ignore trailing bytes after the last known field
// (forward compatibility, AVF.md §1).
package avf
