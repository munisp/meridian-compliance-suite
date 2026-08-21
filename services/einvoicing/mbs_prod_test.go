package main

import (
	"os"
	"os/exec"
	"testing"
)

// QA-22: PROFILE=prod without MBS_BASE_URL must hard-fail (log.Fatal ->
// non-zero exit) rather than fall back to the sandbox simulator.
func TestMBSProdWithoutBaseURLFatals(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		NewMBSClient()
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMBSProdWithoutBaseURLFatals")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "PROFILE=prod", "MBS_BASE_URL=")
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-zero exit (log.Fatal) for PROFILE=prod without MBS_BASE_URL")
	}
}

// Prod with an explicit MBS_BASE_URL selects the real HTTP rail.
func TestMBSProdWithBaseURLUsesHTTP(t *testing.T) {
	t.Setenv("PROFILE", "prod")
	t.Setenv("MBS_BASE_URL", "https://mbs.example.test")
	c := NewMBSClient()
	if _, ok := c.(*HTTPMBS); !ok {
		t.Fatalf("prod client = %T, want *HTTPMBS", c)
	}
}

// Dev keeps the sandbox simulator.
func TestMBSDevKeepsSandbox(t *testing.T) {
	t.Setenv("PROFILE", "")
	t.Setenv("MBS_BASE_URL", "")
	if _, ok := NewMBSClient().(*SandboxMBS); !ok {
		t.Fatalf("dev client = %T, want *SandboxMBS", NewMBSClient())
	}
}
