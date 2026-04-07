package mavlink

import (
	"github.com/hugh/go-drone-server/pkg/protocol"
)

// ValidateGPS checks GPS position bounds and clamps/rejects invalid values.
// Returns false if the position is completely invalid and should be dropped.
func ValidateGPS(gps *protocol.GPSPosition) bool {
	if gps == nil {
		return false
	}
	if gps.Latitude < -90 || gps.Latitude > 90 {
		return false
	}
	if gps.Longitude < -180 || gps.Longitude > 180 {
		return false
	}
	if gps.Altitude > 100000 { // 100km
		return false
	}
	if gps.Heading < 0 || gps.Heading >= 360 {
		// Clamp heading to [0, 360)
		for gps.Heading < 0 {
			gps.Heading += 360
		}
		for gps.Heading >= 360 {
			gps.Heading -= 360
		}
	}
	return true
}

// ValidateBattery checks battery status bounds.
// Returns false if completely invalid.
func ValidateBattery(bat *protocol.BatteryStatus) bool {
	if bat == nil {
		return false
	}
	// Remaining is int8; -1 means unknown, 0-100 is valid
	if bat.Remaining > 100 {
		return false
	}
	return true
}

// ValidateSystemID checks that the system ID is in the valid MAVLink range.
func ValidateSystemID(id uint8) bool {
	return id >= 1 && id <= 250
}
