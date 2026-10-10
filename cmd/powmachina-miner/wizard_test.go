package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"powmachina/internal/address"
	"powmachina/internal/chain"
)

func testAddr(t *testing.T, hrp string) string {
	t.Helper()
	a, err := address.Encode(hrp, chain.ScriptVersionBasic, make([]byte, chain.PubkeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func wiz(t *testing.T, env wizardEnv, answers ...string) (wizardResult, string) {
	t.Helper()
	var out bytes.Buffer
	res, err := runWizard(strings.NewReader(strings.Join(answers, "\n")+"\n"), &out, env)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return res, out.String()
}

var rig = wizardEnv{Threads: 8, MaxCPU: 16, GPUs: []string{"RTX 4080, 17 GB"}, Hostname: "my rig.local"}

// Enter all the way through, apart from the two things nobody can guess.
func TestWizardPoolWithDefaults(t *testing.T) {
	addr := testAddr(t, "tpowm")
	res, _ := wiz(t, rig, "", "", "pool.example.com", addr, "", "", "", "")
	want := []string{"--testnet", "--pool=stratum+tcp://pool.example.com:17699", "--address=" + addr,
		"--worker=myriglocal", "--gpu=on"}
	if !reflect.DeepEqual(res.Args, want) {
		t.Errorf("args\n got %q\nwant %q", res.Args, want)
	}
	if !res.Save {
		t.Error("Enter on the save question has to mean yes")
	}
}

func TestWizardSoloMainnetGPUOnly(t *testing.T) {
	addr := testAddr(t, "powm")
	res, _ := wiz(t, rig, "1", "2", "10.0.0.5", "secret", addr, "2", "n")
	want := []string{"--node=10.0.0.5:7697", "--address=" + addr, "--gpu=on", "--cpu=false"}
	if !reflect.DeepEqual(res.Args, want) {
		t.Errorf("args\n got %q\nwant %q", res.Args, want)
	}
	if res.Token != "secret" || res.Save {
		t.Errorf("token %q, save %v", res.Token, res.Save)
	}
	for _, a := range res.Args {
		if strings.Contains(a, "secret") {
			t.Error("the RPC token must never end up among the saved flags")
		}
	}
}

// A wrong answer is asked again with a reason, not accepted and not fatal.
func TestWizardAsksAgain(t *testing.T) {
	addr := testAddr(t, "tpowm")
	res, out := wiz(t, wizardEnv{Threads: 4, MaxCPU: 4, GPUNote: "no NVIDIA card was found", HasToken: true},
		"9", "2", // network
		"x", "1", // mode
		"http://pool", "stratum+ssl://pool.example.com:443/", // pool
		testAddr(t, "powm"), "tpowm1qqq", addr, // address
		"rig 1", "rig-1", // worker
		"99", "2", // threads
		"maybe", "y")
	want := []string{"--testnet", "--pool=stratum+ssl://pool.example.com:443", "--address=" + addr,
		"--worker=rig-1", "--gpu=off", "--threads=2"}
	if !reflect.DeepEqual(res.Args, want) {
		t.Errorf("args\n got %q\nwant %q", res.Args, want)
	}
	for _, s := range []string{"That is a mainnet address", "not a valid testnet address", "no NVIDIA card was found",
		"Only stratum+tcp:// and stratum+ssl://"} {
		if !strings.Contains(out, s) {
			t.Errorf("the output does not say %q", s)
		}
	}
}

// Input that ends mid-way (a closed pipe) is an error, not an endless loop.
func TestWizardEndOfInput(t *testing.T) {
	var out bytes.Buffer
	if _, err := runWizard(strings.NewReader("2\n1\n"), &out, rig); err == nil {
		t.Fatal("a wizard that ran out of input must fail")
	}
}

func TestConfRoundTrip(t *testing.T) {
	args := []string{"--testnet", "--pool=stratum+tcp://p:17699", "--gpu=on"}
	if got := parseConf(formatConf(args)); !reflect.DeepEqual(got, args) {
		t.Errorf("got %q", got)
	}
	if got := parseConf("# c\r\n\r\n  --testnet \r\n"); !reflect.DeepEqual(got, []string{"--testnet"}) {
		t.Errorf("got %q", got)
	}
}

func TestHostPort(t *testing.T) {
	for in, want := range map[string]string{
		"node": "node:7699", "node:1": "node:1", "1.2.3.4": "1.2.3.4:7699", "[::1]": "[::1]:7699", "[::1]:5": "[::1]:5",
		"": "", "a b": "", "node:x": "", "node:0": "", "::1": "", "a/b": "",
	} {
		got, err := hostPort(in, 7699)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("hostPort(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
