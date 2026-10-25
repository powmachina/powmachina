package main

import "testing"

// Without a token the RPC may only listen on loopback.
func TestRPCBindNeedsATokenOffLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:17697":  true,
		"127.0.0.1:0":      true,
		"[::1]:17697":      true,
		"localhost:17697":  true,
		"0.0.0.0:17697":    false,
		"[::]:17697":       false,
		":17697":           false,
		"10.0.0.5:17697":   false,
		"example.com:7697": false,
		"no-port":          false,
	} {
		if err := checkRPCBind(addr, ""); (err == nil) != ok {
			t.Errorf("%s without a token: %v", addr, err)
		}
		if addr != "no-port" {
			if err := checkRPCBind(addr, "secret"); err != nil {
				t.Errorf("%s with a token: %v", addr, err)
			}
		}
	}
}
