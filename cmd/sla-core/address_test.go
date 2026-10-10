package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCoreAddressDefaultsAndOverrides(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCoreAddressHelper$")
	cmd.Env = append(os.Environ(), "SLA_TEST_MAIN_MODE=help", "SLA_ADDR=")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), `(default "127.0.0.1:8080")`) {
		t.Fatal("program does not default to loopback")
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(200)
	}))
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")
	for _, override := range []bool{false, true} {
		cmd := exec.Command(exe, "-test.run=^TestCoreAddressHelper$")
		cmd.Env = append(os.Environ(), "SLA_TEST_MAIN_MODE=healthcheck", "SLA_ADDR="+addr, "SLA_TEST_ADDR_FLAG=")
		if override {
			cmd.Env = append(cmd.Env, "SLA_ADDR=127.0.0.1:0", "SLA_TEST_ADDR_FLAG="+addr)
		}
		if _, err := cmd.CombinedOutput(); err != nil {
			t.Fatal("explicit address override did not reach the health endpoint")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("SLA_ADDR or -addr was ignored")
	}
}

func TestCoreAddressHelper(t *testing.T) {
	mode := os.Getenv("SLA_TEST_MAIN_MODE")
	if mode == "" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("sla-core", flag.ExitOnError)
	os.Args = []string{"sla-core", "-healthcheck"}
	if mode == "help" {
		os.Args = []string{"sla-core", "-h"}
	}
	if addr := os.Getenv("SLA_TEST_ADDR_FLAG"); addr != "" {
		os.Args = append(os.Args, "-addr", addr)
	}
	main()
}
