package store

import (
	"strconv"
	"strings"

	"airview/internal/model"
)

// Namespaced statistics keys (DESIGN §6.6).
//
// Every per-antenna statistics table has an "ant" column. In airview.db
// (standalone mode) it holds the configured antenna ID, "" meaning all
// antennas combined. In stations.db (aggregator mode) the same tables hold
// every station, namespaced by a prefix:
//
//	"s<idx>"        the station combined     (AntKey("s7", "")  = "s7")
//	"s<idx>/<ch>"   one antenna channel      (AntKey("s7", "1") = "s7/1")
//
// model.StationAntKey builds the same keys from a StationIdx and channel.
// A station's keys are the key "s<idx>" itself plus the key range
// ["s<idx>/", "s<idx>0") ('0' is the byte after '/'), so "s7" never matches
// "s70": see AntKeyRange and AntKeyWhere.

// StationAntPrefix returns the key prefix of a station: "s<idx>".
func StationAntPrefix(idx model.StationIdx) string { return model.StationAntKey(idx, -1) }

// AntKey joins a key prefix and an antenna ID: AntKey("", ant) = ant
// (standalone), AntKey(p, "") = p, otherwise p + "/" + ant.
func AntKey(prefix, ant string) string {
	switch {
	case prefix == "":
		return ant
	case ant == "":
		return prefix
	}
	return prefix + "/" + ant
}

// SplitAntKey is the inverse of AntKey for a namespaced key: "s7/1" ->
// ("s7", "1"), "s7" -> ("s7", ""). A key that is not namespaced (a
// standalone antenna ID) returns ("", key).
func SplitAntKey(key string) (prefix, ant string) {
	head, tail, found := strings.Cut(key, "/")
	if !ValidAntPrefix(head) {
		return "", key
	}
	if !found {
		return head, ""
	}
	return head, tail
}

// ValidAntPrefix reports whether p is a station prefix "s<idx>" with a
// decimal index > 0 and no leading zero.
func ValidAntPrefix(p string) bool {
	if len(p) < 2 || p[0] != 's' || p[1] == '0' {
		return false
	}
	_, err := strconv.ParseUint(p[1:], 10, 32)
	return err == nil
}

// AntKeyRange returns the half-open range [lo, hi) that holds the channel
// keys of prefix ("s7/" .. "s70"); the prefix key itself is not inside it.
func AntKeyRange(prefix string) (lo, hi string) { return prefix + "/", prefix + "0" }

// AntKeyWhere returns an SQL condition (and its arguments) that selects
// every key of prefix in column col: the combined key and all channel keys.
// col must be a trusted identifier.
func AntKeyWhere(col, prefix string) (string, []any) {
	lo, hi := AntKeyRange(prefix)
	return "(" + col + " = ? OR (" + col + " >= ? AND " + col + " < ?))", []any{prefix, lo, hi}
}

// StationFeedRange returns the half-open range of the FeedIDs of a station
// ('st/<sid>/<rx>'): ["st/<sid>/", "st/<sid>0").
func StationFeedRange(sid string) (lo, hi string) { return "st/" + sid + "/", "st/" + sid + "0" }
