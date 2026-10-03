//go:build !linux && !darwin

package sidecar

import "fmt"

// FixtureEnv is the native group fixture's variable (unsupported here).
const FixtureEnv = "CALLSHEET_SIDECAR_FIXTURE"

func runFixture(string) int {
	fmt.Println("no sidecar fixture on this system")
	return 2
}

// probeCalibrationEnv selects the probe calibration (unsupported here).
const probeCalibrationEnv = "CALLSHEET_SIDECAR_PROBE_CALIBRATION"

func runInjectedGuardian([]string) int { return 2 }

func probeCalibration() int { return 2 }
