package controller

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// yamlSingleQuote formats s as a single-quoted YAML scalar. Go's %q escapes
// non-BMP characters (e.g. emoji) as \Uxxxxxxxx, and so does every YAML library
// tried here (sigs.k8s.io/yaml, gopkg.in/yaml.v2, gopkg.in/yaml.v3 — even when
// forced to SingleQuotedStyle) — ThunderID's YAML parser doesn't support that
// escape form (see APP-1018). Hand-writing every scalar as a single-quoted
// literal sidesteps all of them: single-quote style only ever needs to escape
// the quote character itself, never the content.
func yamlSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// yamlScalar renders v (a leaf value out of a decoded JSON document — so only
// string/bool/nil/float64 ever reach here) as a YAML scalar.
func yamlScalar(v interface{}) string {
	switch val := v.(type) {
	case string:
		return yamlSingleQuote(val)
	case bool:
		return fmt.Sprintf("%v", val)
	case nil:
		return "null"
	case float64:
		// encoding/json decodes every JSON number as float64; render whole numbers
		// without a trailing ".0" so integer fields (e.g. maxOpenConns) round-trip clean.
		if !math.IsInf(val, 0) && val == math.Trunc(val) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// writeYAMLMapBody writes each of data's keys, sorted, as "<indent><key>: ...\n" lines.
func writeYAMLMapBody(buf *bytes.Buffer, indent string, data map[string]interface{}) {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeYAMLField(buf, indent, k, data[k])
	}
}

// writeYAMLField writes one "<indent><key>: <scalar>\n" line, or "<indent><key>:\n"
// followed by the nested map/list content for a composite value.
func writeYAMLField(buf *bytes.Buffer, indent, key string, v interface{}) {
	switch val := v.(type) {
	case map[string]interface{}:
		if len(val) == 0 {
			fmt.Fprintf(buf, "%s%s: {}\n", indent, key)
			return
		}
		fmt.Fprintf(buf, "%s%s:\n", indent, key)
		writeYAMLMapBody(buf, indent+"  ", val)
	case []interface{}:
		if len(val) == 0 {
			fmt.Fprintf(buf, "%s%s: []\n", indent, key)
			return
		}
		fmt.Fprintf(buf, "%s%s:\n", indent, key)
		for _, item := range val {
			writeYAMLListItem(buf, indent, item)
		}
	default:
		fmt.Fprintf(buf, "%s%s: %s\n", indent, key, yamlScalar(val))
	}
}

// writeYAMLListItem writes one "<indent>- ..." block-sequence entry. A map item's first
// field shares the "- " line; the rest align two spaces past it, matching the style every
// hand-written list in this package (permissions, members, resources, ...) already used.
func writeYAMLListItem(buf *bytes.Buffer, indent string, v interface{}) {
	switch val := v.(type) {
	case map[string]interface{}:
		if len(val) == 0 {
			fmt.Fprintf(buf, "%s- {}\n", indent)
			return
		}
		var body bytes.Buffer
		writeYAMLMapBody(&body, indent+"  ", val)
		lines := strings.SplitAfter(body.String(), "\n")
		for i, line := range lines {
			if line == "" {
				continue
			}
			if i == 0 {
				buf.WriteString(indent + "- " + strings.TrimPrefix(line, indent+"  "))
			} else {
				buf.WriteString(line)
			}
		}
	case []interface{}:
		if len(val) == 0 {
			fmt.Fprintf(buf, "%s- []\n", indent)
			return
		}
		fmt.Fprintf(buf, "%s-\n", indent)
		for _, item := range val {
			writeYAMLListItem(buf, indent+"  ", item)
		}
	default:
		fmt.Fprintf(buf, "%s- %s\n", indent, yamlScalar(val))
	}
}

// deterministicUUIDv7 converts a Kubernetes object's identity (UID + creation time) into a
// UUIDv7-shaped string. Kubernetes UIDs are already random (UUIDv4), but ThunderID requires
// v7 (time-ordered) identifiers for some resource types; this derives one deterministically
// so the same CR always maps to the same ThunderID id across reconciles.
func deterministicUUIDv7(meta metav1.ObjectMeta) string {
	tsMs := meta.CreationTimestamp.UnixMilli()
	h := sha256.Sum256([]byte(string(meta.UID)))

	var b [16]byte
	b[0] = byte(tsMs >> 40)
	b[1] = byte(tsMs >> 32)
	b[2] = byte(tsMs >> 24)
	b[3] = byte(tsMs >> 16)
	b[4] = byte(tsMs >> 8)
	b[5] = byte(tsMs)
	b[6] = 0x70 | (h[0] & 0x0F) // version nibble = 7
	b[7] = h[1]
	b[8] = 0x80 | (h[2] & 0x3F) // variant = 10xx
	copy(b[9:], h[3:10])

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
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
