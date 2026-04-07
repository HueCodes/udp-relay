package mavlink

// CRC-16/MCRF4XX (X.25) checksum used by MAVLink v2.
// Polynomial: 0x1021, init: 0xFFFF

// crcSeed contains per-message CRC_EXTRA bytes from MAVLink definitions.
// These ensure the sender and receiver agree on the message format.
var crcSeed = map[uint32]byte{
	0:   50,  // HEARTBEAT
	1:   124, // SYS_STATUS
	24:  24,  // GPS_RAW_INT
	30:  39,  // ATTITUDE
	33:  104, // GLOBAL_POSITION_INT
	147: 154, // BATTERY_STATUS
	245: 130, // EXTENDED_SYS_STATE
}

// crcAccumulate adds one byte to the running CRC.
func crcAccumulate(b byte, crc uint16) uint16 {
	tmp := uint16(b) ^ (crc & 0xFF)
	tmp ^= (tmp << 4) & 0xFF
	return (crc >> 8) ^ (tmp << 8) ^ (tmp << 3) ^ (tmp >> 4)
}

// crcCalculate computes the CRC-16/MCRF4XX over buf with the given seed byte.
func crcCalculate(buf []byte, seed byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range buf {
		crc = crcAccumulate(b, crc)
	}
	// Accumulate the CRC_EXTRA seed
	crc = crcAccumulate(seed, crc)
	return crc
}
