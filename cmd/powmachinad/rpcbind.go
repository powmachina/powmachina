package main

import (
	"errors"
	"net"
	"strings"
)

// checkRPCBind refuses an RPC address other machines can reach when there is no token: without
// one anybody who reaches the port could call every method (audit 2026-10-25, PM-04). Only a
// loopback address may go without a token; an empty host, 0.0.0.0, :: and any name other than
// localhost count as reachable, because what they resolve to is not certain.
func checkRPCBind(addr, token string) error {
	if token != "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("the RPC on " + addr + " is reachable from other machines and POWM_RPC_TOKEN is empty — set a token, or bind to 127.0.0.1")
}
