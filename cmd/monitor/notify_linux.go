package main

import (
	"net"
	"os"
	"strings"
)

// notifyReady tells systemd the service is up, which is what Type=notify units
// wait for before considering the start-up complete. It is a no-op when the
// process was not started by systemd.
func notifyReady() {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	// A leading '@' denotes the abstract socket namespace, encoded as a NUL.
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:]
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()

	conn.Write([]byte("READY=1"))
}
