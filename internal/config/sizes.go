package config

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// fileConfig overrides only the two byte fields at the JSON boundary.
// Config and the receiver continue to use integer bytes.
type fileConfig struct {
	*Config
	MaxPayloadBytes json.RawMessage `json:"max_payload_bytes"`
	MaxDropBytes    json.RawMessage `json:"max_drop_bytes"`
}

var sizeUnits = []struct {
	name  string
	bytes int64
}{
	{"TiB", 1 << 40}, {"TB", 1_000_000_000_000},
	{"GiB", 1 << 30}, {"GB", 1_000_000_000},
	{"MiB", 1 << 20}, {"MB", 1_000_000},
	{"KiB", 1 << 10}, {"KB", 1_000},
	{"B", 1},
}

func parseSize(raw json.RawMessage, key string) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	invalid := fmt.Errorf("%s: use integer bytes or a quoted whole-number size such as \"5 GiB\"; units: B, KB, MB, GB, TB, KiB, MiB, GiB, TiB", key)
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, invalid
	}
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return 0, invalid
	}
	for _, c := range fields[0] {
		if c < '0' || c > '9' {
			return 0, invalid
		}
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, invalid
	}
	for _, unit := range sizeUnits {
		if strings.EqualFold(fields[1], unit.name) {
			if n > math.MaxInt64/unit.bytes {
				return 0, fmt.Errorf("%s: size exceeds the integer byte range", key)
			}
			return n * unit.bytes, nil
		}
	}
	return 0, invalid
}

// Write exact whole-unit values; never round a configured limit.
func sizeJSON(n int64) json.RawMessage {
	if n > 0 {
		for _, unit := range sizeUnits {
			if unit.bytes > 1 && n%unit.bytes == 0 {
				return json.RawMessage(strconv.Quote(fmt.Sprintf("%d %s", n/unit.bytes, unit.name)))
			}
		}
	}
	return json.RawMessage(strconv.FormatInt(n, 10))
}
