// Package main provides a MAVLink frame replay tool.
// It reads length-prefixed MAVLink v2 frames from a binary capture file
// and replays them over UDP at the original timing or a configurable speed.
//
// File format: sequence of [uint32 delay_us][uint16 frame_len][frame_bytes...]
package main

import (
	"encoding/binary"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		file     = flag.String("file", "", "Path to capture file (required)")
		target   = flag.String("target", "127.0.0.1:14550", "Target UDP address")
		speed    = flag.Float64("speed", 1.0, "Playback speed multiplier (2.0 = 2x faster)")
		loop     = flag.Bool("loop", false, "Loop playback continuously")
		generate = flag.Bool("generate", false, "Generate a sample capture file instead of replaying")
		duration = flag.Int("duration", 60, "Duration in seconds for generated capture")
		out      = flag.String("out", "testdata/sample_flight.bin", "Output path for generated capture")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *generate {
		logger.Info("generating sample capture", "path", *out, "duration_sec", *duration)
		if err := generateCapture(*out, *duration); err != nil {
			logger.Error("generation failed", "error", err)
			os.Exit(1)
		}
		logger.Info("capture file generated", "path", *out)
		return
	}

	if *file == "" {
		logger.Error("capture file required: use -file <path>")
		flag.Usage()
		os.Exit(1)
	}

	if *speed <= 0 {
		logger.Error("speed must be positive")
		os.Exit(1)
	}

	addr, err := net.ResolveUDPAddr("udp", *target)
	if err != nil {
		logger.Error("invalid target", "error", err)
		os.Exit(1)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		logger.Error("connect failed", "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logger.Info("replay starting",
		"file", *file,
		"target", *target,
		"speed", *speed,
		"loop", *loop,
	)

	totalSent := 0
	passes := 0

	for {
		f, err := os.Open(*file)
		if err != nil {
			logger.Error("failed to open file", "error", err)
			os.Exit(1)
		}

		sent, err := replayFile(f, conn, *speed, sigChan, logger)
		f.Close()
		totalSent += sent
		passes++

		if err != nil {
			if err == errInterrupted {
				break
			}
			logger.Error("replay error", "error", err)
			break
		}

		logger.Info("pass complete", "pass", passes, "frames_sent", sent)

		if !*loop {
			break
		}
	}

	logger.Info("replay finished", "total_sent", totalSent, "passes", passes)
}

var errInterrupted = io.EOF

func replayFile(r io.Reader, conn *net.UDPConn, speed float64, sigChan <-chan os.Signal, logger *slog.Logger) (int, error) {
	var header [6]byte // delay_us(4) + frame_len(2)
	sent := 0

	for {
		// Check for interrupt
		select {
		case <-sigChan:
			return sent, errInterrupted
		default:
		}

		// Read frame header
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return sent, nil
			}
			return sent, err
		}

		delayUS := binary.LittleEndian.Uint32(header[0:4])
		frameLen := binary.LittleEndian.Uint16(header[4:6])

		if frameLen == 0 || frameLen > 300 {
			logger.Warn("invalid frame length, skipping", "len", frameLen)
			continue
		}

		// Read frame data
		frame := make([]byte, frameLen)
		if _, err := io.ReadFull(r, frame); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return sent, nil
			}
			return sent, err
		}

		// Apply timing delay (adjusted by speed multiplier)
		if delayUS > 0 && speed > 0 {
			delay := time.Duration(float64(delayUS) / speed * float64(time.Microsecond))
			if delay > 0 && delay < 10*time.Second {
				time.Sleep(delay)
			}
		}

		if _, err := conn.Write(frame); err != nil {
			logger.Debug("send error", "error", err)
		}
		sent++

		if sent%1000 == 0 {
			logger.Info("progress", "frames_sent", sent)
		}
	}
}
