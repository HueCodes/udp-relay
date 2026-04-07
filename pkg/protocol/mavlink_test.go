package protocol

import "testing"

func TestMAVType_String(t *testing.T) {
	tests := []struct {
		t    MAVType
		want string
	}{
		{MAVTypeGeneric, "generic"},
		{MAVTypeFixedWing, "fixed_wing"},
		{MAVTypeQuadrotor, "quadrotor"},
		{MAVTypeHexarotor, "hexarotor"},
		{MAVTypeOctorotor, "octorotor"},
		{MAVTypeGroundRover, "ground_rover"},
		{MAVTypeSubmarine, "submarine"},
		{MAVType(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.t.String(); got != tt.want {
			t.Errorf("MAVType(%d).String() = %q, want %q", tt.t, got, tt.want)
		}
	}
}

func TestMAVState_String(t *testing.T) {
	tests := []struct {
		s    MAVState
		want string
	}{
		{MAVStateUninit, "uninitialized"},
		{MAVStateBoot, "boot"},
		{MAVStateCalibrating, "calibrating"},
		{MAVStateStandby, "standby"},
		{MAVStateActive, "active"},
		{MAVStateCritical, "critical"},
		{MAVStateEmergency, "emergency"},
		{MAVStatePoweroff, "poweroff"},
		{MAVState(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("MAVState(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestGPSFixType_String(t *testing.T) {
	tests := []struct {
		f    GPSFixType
		want string
	}{
		{GPSFixNone, "no_fix"},
		{GPSFix2D, "2d_fix"},
		{GPSFix3D, "3d_fix"},
		{GPSFixDGPS, "dgps"},
		{GPSFixRTK, "rtk"},
		{GPSFixType(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.f.String(); got != tt.want {
			t.Errorf("GPSFixType(%d).String() = %q, want %q", tt.f, got, tt.want)
		}
	}
}
