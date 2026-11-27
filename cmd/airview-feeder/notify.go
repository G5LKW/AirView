package main

import (
	"net"
	"os"
	"strconv"
	"time"
)

// systemd readiness protocol (sd_notify), implemented directly: a datagram
// to $NOTIFY_SOCKET. Used with Type=notify units; harmless elsewhere (the
// variable is unset, or the socket type does not exist on the platform).

func sdNotify(state string) {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return
	}
	if path[0] == '@' {
		path = "\x00" + path[1:] // abstract socket
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return
	}
	defer c.Close()
	_, _ = c.Write([]byte(state))
}

// sdWatchdogInterval returns half the systemd watchdog period (0 = none).
func sdWatchdogInterval() time.Duration {
	us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || us <= 0 {
		return 0
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(us) * time.Microsecond / 2
}
