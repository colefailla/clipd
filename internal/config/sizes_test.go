package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSizeConfigInputs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{`1231234523`, 1231234523}, {`"5 GB"`, 5_000_000_000}, {`"5 gib"`, 5 << 30},
		{`"500 MB"`, 500_000_000}, {`"1500 MiB"`, 1500 << 20}, {`"1 kb"`, 1000},
		{`"1 KiB"`, 1024}, {`"1 TB"`, 1_000_000_000_000}, {`"1 TiB"`, 1 << 40},
		{`"123 B"`, 123}, {`" 5   GiB "`, 5 << 30}, {`0`, DefaultMaxDropBytes},
		{`"0 B"`, DefaultMaxDropBytes}, {`null`, DefaultMaxDropBytes},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(`{"max_drop_bytes":`+tc.raw+`}`), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil || cfg.MaxDropBytes != tc.want {
				t.Fatalf("Load: %+v, %v; want %d", cfg, err, tc.want)
			}
		})
	}
}

func TestSizeConfigRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"max_payload_bytes", "max_drop_bytes"} {
		for _, raw := range []string{`"5"`, `"5GiB"`, `"1.5 GiB"`, `"-1 GB"`, `"+5 GB"`, `"5 G"`, `"5 Gb/s"`, `""`, `true`, `[]`, `{}`, `1.5`, `9223372036854775808`, `"9223372036854775808 B"`, `"9223372036854775807 GiB"`} {
			t.Run(key+raw, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(path, []byte(fmt.Sprintf(`{%q:%s}`, key, raw)), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := Load(path); err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("expected setting-specific error, got %v", err)
				}
			})
		}
	}
}

func TestReadableSizesKeepValidation(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"max_payload_bytes":"2 GiB"}`, `{"max_drop_bytes":"2 TiB"}`,
		`{"max_payload_bytes":"1 GiB","max_concurrent":8}`,
		`{"max_drop_bytes":"5 GiB","unknown":1}`,
		`{"max_drop_bytes":"5 GiB"} {}`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestSaveSizesAreReadableAndExact(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		n       int64
		encoded string
	}{
		{5 << 30, `"5 GiB"`}, {5_000_000_000, `"5 GB"`},
		{1_000_000_000_000, `"1 TB"`}, {1231234523, `1231234523`},
	} {
		t.Run(tc.encoded, func(t *testing.T) {
			cfg := Default()
			cfg.MaxDropBytes = tc.n
			path := filepath.Join(t.TempDir(), "config.json")
			if err := cfg.Save(path); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["max_drop_bytes"]) != tc.encoded || string(fields["max_payload_bytes"]) != `"10 MiB"` {
				t.Fatalf("saved sizes: %s", data)
			}
			loaded, err := Load(path)
			if err != nil || loaded != cfg {
				t.Fatalf("round trip: %+v, %v", loaded, err)
			}
		})
	}
}
