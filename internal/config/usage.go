package config

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// Var documents one environment variable. Vars is the single list of every
// variable Load reads (a test enforces that), and is what `phonetic -h`
// prints.
type Var struct {
	Name    string
	Default string // "" means unset by default
	Help    string
}

// Vars lists every environment variable phonetic reads, in display order.
var Vars = []Var{
	{"PHONETIC_API_TOKEN", "(required)", "bearer token for /api/; at least 16 characters"},
	{"PHONETIC_ADDR", DefaultAddr, "host:port to listen on, with an explicit port (in production, the Tailscale IP)"},
	{"PHONETIC_DATA_DIR", DefaultDataDir, "database and article files; relative paths resolve against the working directory (/ under systemd)"},
	{"PHONETIC_LOG_LEVEL", "info", "debug, info, warn or error"},
	{"PHONETIC_WORKERS", fmt.Sprint(DefaultWorkers), fmt.Sprintf("pipeline workers (1-%d)", MaxWorkers)},
	{"PHONETIC_FETCH_BLOCKED", "", "comma-separated addresses or CIDRs the fetcher must never connect to, e.g. your WAN IP (IPv4 /8 or longer, IPv6 /16 or longer)"},
	{"PHONETIC_FETCH_ALLOW_PRIVATE", "false", "DEV/TEST ONLY: allow fetching private and local addresses (disables SSRF protection; requires a loopback PHONETIC_ADDR)"},
	{"PHONETIC_DEV_STUB_DELAY", "0s", "DEV ONLY: make the placeholder pipeline stages sleep this long"},
}

// WriteUsage writes a table of Vars to w.
func WriteUsage(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  VARIABLE\tDEFAULT\tDESCRIPTION")
	for _, v := range Vars {
		def := v.Default
		if def == "" {
			def = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", v.Name, def, v.Help)
	}
	return tw.Flush()
}
