package main

import (
	"strings"
	"testing"
)

// 期望值由官方 CLI 1.53.0 buildMachineFingerprint 公式独立计算
// （python hashlib，salt "command-code:device-fingerprint:v1"）。
func TestFingerprintOfficialFormula(t *testing.T) {
	machine := "AB12CD34-5678-90EF-GH12-IJKL34567890"
	macs := []string{"0a:1b:2c:3d:4e:5f", "10:20:30:40:50:60"}

	if got := fpHash(machine); got != "25ec252ab645100a93b1c0414dea7eb567eda56ad5babd49cba384f9e2dca511" {
		t.Errorf("fpHash(machineId) = %s", got)
	}
	if got := fpHash(macs[0]); got != "4f1913d674cd477f5edce1f8cd07ccc549bc3afc56b0deafaa9f9ba94e012f75" {
		t.Errorf("fpHash(mac) = %s", got)
	}
	// 大小写归一：CLI hashSignal 会 lower(trim(v))
	if got := fpHash(strings.ToUpper(machine)); got != fpHash(strings.ToLower(machine)) {
		t.Error("fpHash should normalize case")
	}
	// 空值返回空串（CLI 返回 undefined → JSON 省略字段）
	if fpHash("   ") != "" {
		t.Error("fpHash(whitespace) should be empty")
	}
	// 有 machineId：hostname/cpuModel 不参与 thumbmark
	th := fpThumbmark(machine, macs, "DESKTOP-XYZ", "some cpu")
	if th != "ba5bb2e34ac09c5a07910aa88ee24d7e7d27cfd8ffabdf31d3b0668005c2d241" {
		t.Errorf("fpThumbmark(with machineId) = %s", th)
	}
	if th2 := fpThumbmark(machine, macs, "OTHER-HOST", "other cpu"); th2 != th {
		t.Error("hostname/cpuModel must not affect thumbmark when machineId present")
	}
	// 无 machineId：[macs, hostname, cpuModel]
	if got := fpThumbmark("", []string{"aa:bb:cc:dd:ee:ff"}, "myhost", "Intel(R) Core(TM) i9-13900K"); got != "9c8e525c74ffeb0a0511dcf8c60dfb450a0bafcd0b815dae6909231f9dd80689" {
		t.Errorf("fpThumbmark(no machineId) = %s", got)
	}
}

func TestNewFingerprintShape(t *testing.T) {
	for i := 0; i < 50; i++ {
		fp := newFingerprint()
		c := fp.Components
		if len(fp.Thumbmark) != 64 {
			t.Fatalf("thumbmark not sha256 hex: %s", fp.Thumbmark)
		}
		if len(c.MachineIDHash) != 64 || c.MachineIDHash == fpHash(c.MachineIDHash) {
			t.Fatalf("machineIdHash not a hash of raw value")
		}
		if len(c.MacHashes) < 2 || len(c.MacHashes) > 5 {
			t.Fatalf("macHashes count = %d", len(c.MacHashes))
		}
		if c.Runtime != "cli" || c.CollectorVersion != 1 {
			t.Fatalf("runtime/collectorVersion = %s/%d", c.Runtime, c.CollectorVersion)
		}
		switch c.Platform {
		case "win32":
			if !strings.HasPrefix(c.OSRelease, "10.0.") || c.Arch != "x64" {
				t.Fatalf("win32 shape: %s/%s", c.OSRelease, c.Arch)
			}
		case "darwin":
			if c.Arch != "arm64" || !strings.HasPrefix(c.CPUModel, "Apple") {
				t.Fatalf("darwin shape: %s/%s", c.CPUModel, c.Arch)
			}
		case "linux":
			if c.IsContainer && c.Platform != "linux" {
				t.Fatal("isContainer only on linux")
			}
		default:
			t.Fatalf("unexpected platform %s", c.Platform)
		}
		if c.CPUCount < 4 || c.CPUCount > 24 || c.MemGiB < 8 {
			t.Fatalf("cpu/mem implausible: %d/%d", c.CPUCount, c.MemGiB)
		}
		if c.Timezone == "" || !strings.Contains(c.Timezone, "/") {
			t.Fatalf("timezone = %q", c.Timezone)
		}
	}
}
