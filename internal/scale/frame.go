// Package scale reads a truck-scale indicator's continuous output and decides
// when its weight is steady.
package scale

import (
	"bytes"
	"errors"
	"strconv"
)

// Status is what the indicator says about the weight on the line it sent.
type Status string

const (
	StatusOK       Status = "ok"       // valid, not moving
	StatusMotion   Status = "motion"   // the weight is still changing
	StatusZero     Status = "zero"     // valid, not moving, at the centre of zero
	StatusOverload Status = "overload" // over or under the scale's range
	StatusInvalid  Status = "invalid"  // the indicator itself flags the reading as bad
)

// Frame is one decoded line from the indicator.
type Frame struct {
	Weight float64 // signed, in Unit
	Unit   string  // "lb", "kg", "t", "g", "oz"
	Mode   string  // "gross", "net", "tare"
	Status Status
}

// Still reports whether the indicator considers the weight settled.
func (f Frame) Still() bool { return f.Status == StatusOK || f.Status == StatusZero }

// ErrFrame is returned for a line that is not a scale reading.
var ErrFrame = errors.New("not a scale line")

const (
	stx = 0x02
	// frameLen is the line without its terminator: sign, seven weight
	// characters, unit, mode and status. Rice Lake's standard stream format
	// (`<STX><P><W7.><U><M><S><CR><LF>`), which the B-TEK DD700 at the yard is
	// also set to send.
	frameLen = 1 + 7 + 1 + 1 + 1
)

// ParseFrame decodes one line, with or without its STX and CR LF.
func ParseFrame(line []byte) (Frame, error) {
	line = bytes.TrimRight(line, "\r\n")
	if i := bytes.LastIndexByte(line, stx); i >= 0 {
		line = line[i+1:]
	}
	if len(line) != frameLen {
		return Frame{}, ErrFrame
	}

	sign, digits := line[0], bytes.TrimSpace(line[1:8])
	if (sign != ' ' && sign != '-') || len(digits) == 0 {
		return Frame{}, ErrFrame
	}
	for _, c := range digits {
		if (c < '0' || c > '9') && c != '.' {
			return Frame{}, ErrFrame
		}
	}
	weight, err := strconv.ParseFloat(string(digits), 64)
	if err != nil {
		return Frame{}, ErrFrame
	}
	if sign == '-' {
		weight = -weight
	}

	unit, ok := units[line[8]]
	if !ok {
		return Frame{}, ErrFrame
	}
	mode, ok := modes[line[9]]
	if !ok {
		return Frame{}, ErrFrame
	}
	status, ok := statuses[line[10]]
	if !ok {
		return Frame{}, ErrFrame
	}
	return Frame{Weight: weight, Unit: unit, Mode: mode, Status: status}, nil
}

var units = map[byte]string{'L': "lb", 'K': "kg", 'T': "t", 'G': "g", 'O': "oz"}

var modes = map[byte]string{'G': "gross", 'N': "net", 'T': "tare"}

var statuses = map[byte]Status{
	' ': StatusOK,
	'M': StatusMotion,
	'Z': StatusZero,
	'O': StatusOverload,
	'I': StatusInvalid,
}
