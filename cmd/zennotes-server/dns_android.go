//go:build android

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	if _, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		return
	}
	prefix := os.Getenv("PREFIX")
	if prefix == "" {
		prefix = "/data/data/com.termux/files/usr"
	}
	raw, err := os.ReadFile(filepath.Join(prefix, "etc", "resolv.conf"))
	if err != nil {
		return
	}
	var servers []string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" && net.ParseIP(fields[1]) != nil {
			servers = append(servers, net.JoinHostPort(fields[1], "53"))
		}
	}
	if len(servers) == 0 {
		return
	}
	// Pure-Go DNS otherwise falls back to localhost on Android.
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var err error
			for _, server := range servers {
				var conn net.Conn
				conn, err = (&net.Dialer{}).DialContext(ctx, network, server)
				if err == nil {
					return conn, nil
				}
			}
			return nil, err
		},
	}
}
