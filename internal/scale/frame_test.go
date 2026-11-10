package scale

import (
	"errors"
	"testing"
)

func TestParseFrame(t *testing.T) {
	cases := []struct {
		line string
		want Frame
	}{
		// Lines exactly as the yard's two indicators sent them.
		{"\x02      00LGZ\r\n", Frame{0, "lb", "gross", StatusZero}},
		{"\x02      00LG \r\n", Frame{0, "lb", "gross", StatusOK}},
		{"\x02    2840LGM\r\n", Frame{2840, "lb", "gross", StatusMotion}},
		{"\x02    2840LG \r\n", Frame{2840, "lb", "gross", StatusOK}},
		{"\x02-     20LG \r\n", Frame{-20, "lb", "gross", StatusOK}},
		// Variants the format allows.
		{"\x02   41580LN \r\n", Frame{41580, "lb", "net", StatusOK}},
		{"\x02  1234.5KGO", Frame{1234.5, "kg", "gross", StatusOverload}},
		{"    2840LG ", Frame{2840, "lb", "gross", StatusOK}},
		// A line that began mid-stream keeps only what follows its last STX.
		{"0LG \r\x02    2840LG \r\n", Frame{2840, "lb", "gross", StatusOK}},
	}
	for _, c := range cases {
		got, err := ParseFrame([]byte(c.line))
		if err != nil {
			t.Errorf("ParseFrame(%q) error: %v", c.line, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseFrame(%q) = %+v, want %+v", c.line, got, c.want)
		}
	}
}

func TestParseFrameRejects(t *testing.T) {
	for _, line := range []string{
		"",
		"\x02",
		"\x02    2840LG\r\n",  // one character short
		"\x02    2840XG \r\n", // unknown unit
		"\x02    2840LQ \r\n", // unknown mode
		"\x02    2840LGX\r\n", // unknown status
		"\x02+   2840LG \r\n", // bad sign
		"\x02    28A0LG \r\n", // not a number
		"\x02        LG \r\n", // no digits
		"\x02  12.3.4LG \r\n", // two decimal points
	} {
		if _, err := ParseFrame([]byte(line)); !errors.Is(err, ErrFrame) {
			t.Errorf("ParseFrame(%q) = %v, want ErrFrame", line, err)
		}
	}
}
