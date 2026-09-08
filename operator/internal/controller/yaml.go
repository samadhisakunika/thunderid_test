package controller

import (
	"bytes"
	"slices"

	"gopkg.in/yaml.v3"
)

// renderResourceYAML renders data (a decoded JSON document — so only string/bool/nil/float64/
// map[string]any/[]any values appear in it) as a YAML document via gopkg.in/yaml.v3, the same
// library ThunderID's own declarative resource parsers (application, theme, flow, ou, role, ...)
// use to read it back. An earlier version of this function hand-wrote YAML instead, because every
// library tried — including this one — escapes non-BMP runes (emoji) in a marshaled string as
// \Uxxxxxxxx, and ThunderID's parser was believed unable to read that escape form (APP-1018).
// Confirmed live against a running ThunderID instance (2026-09-08) that this is no longer true:
// yaml.v3 both produces and correctly decodes \Uxxxxxxxx scalars today, so the hand-rolled writer
// is no longer needed. Marshal errors are not handled because data only ever contains values that
// came out of encoding/json.Unmarshal, all of which are directly encodable.
func renderResourceYAML(data map[string]any) string {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(data)
	_ = enc.Close()
	return buf.String()
}

func containsString(slice []string, s string) bool {
	return slices.Contains(slice, s)
}

func removeString(slice []string, s string) []string {
	var out []string
	for _, item := range slice {
		if item != s {
			out = append(out, item)
		}
	}
	return out
}
