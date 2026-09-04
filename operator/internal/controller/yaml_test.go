/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"math"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

type YAMLTestSuite struct {
	suite.Suite
}

func TestYAMLTestSuite(t *testing.T) {
	suite.Run(t, new(YAMLTestSuite))
}

func (suite *YAMLTestSuite) TestYAMLSingleQuote() {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "'hello'"},
		{"empty", "", "''"},
		{"single quote escaped", "it's", "'it''s'"},
		{"multiple quotes", "''", "''''''"},
		{"emoji passes through untouched", "👨‍💻", "'👨‍💻'"},
	}
	for _, c := range cases {
		suite.Run(c.name, func() {
			suite.Equal(c.want, yamlSingleQuote(c.in))
		})
	}
}

func (suite *YAMLTestSuite) TestYAMLSingleQuoteRoundTrips() {
	// The whole point of hand-writing scalars (see yaml.go's doc comment, APP-1018) is that a
	// value containing non-BMP runes still parses back to the exact original string.
	in := "emoji:👨‍💻 and 'quotes'"
	quoted := yamlSingleQuote(in)
	doc := "key: " + quoted + "\n"

	var parsed map[string]string
	suite.Require().NoError(yaml.Unmarshal([]byte(doc), &parsed))
	suite.Equal(in, parsed["key"])
}

func (suite *YAMLTestSuite) TestYAMLScalar() {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"string", "abc", "'abc'"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"nil", nil, "null"},
		{"whole float renders without .0", float64(42), "42"},
		{"negative whole float", float64(-7), "-7"},
		{"fractional float keeps precision", float64(3.5), "3.5"},
		{"infinite float skips the whole-number path", math.Inf(1), "+Inf"},
		{"any other type falls through to %v", []string{"x"}, "[x]"},
	}
	for _, c := range cases {
		suite.Run(c.name, func() {
			suite.Equal(c.want, yamlScalar(c.in))
		})
	}
}

func (suite *YAMLTestSuite) TestWriteYAMLMapBody() {
	var buf bytes.Buffer
	writeYAMLMapBody(&buf, "", map[string]interface{}{
		"name": "console",
		"port": float64(8090),
		"tags": []interface{}{"a", "b"},
		"nested": map[string]interface{}{
			"inner": true,
		},
		"empty_map":  map[string]interface{}{},
		"empty_list": []interface{}{},
	})

	// Keys are written sorted, regardless of map iteration order.
	want := "empty_list: []\n" +
		"empty_map: {}\n" +
		"name: 'console'\n" +
		"nested:\n" +
		"  inner: true\n" +
		"port: 8090\n" +
		"tags:\n" +
		"- 'a'\n" +
		"- 'b'\n"
	suite.Equal(want, buf.String())
}

func (suite *YAMLTestSuite) TestWriteYAMLListItemMapAlignment() {
	var buf bytes.Buffer
	writeYAMLListItem(&buf, "", map[string]interface{}{
		"id":   "1",
		"name": "first",
	})
	suite.Equal("- id: '1'\n  name: 'first'\n", buf.String())
}

func (suite *YAMLTestSuite) TestWriteYAMLListItemEmptyMap() {
	var buf bytes.Buffer
	writeYAMLListItem(&buf, "", map[string]interface{}{})
	suite.Equal("- {}\n", buf.String())
}

func (suite *YAMLTestSuite) TestWriteYAMLListItemEmptyNestedList() {
	var buf bytes.Buffer
	writeYAMLListItem(&buf, "", []interface{}{})
	suite.Equal("- []\n", buf.String())
}

func (suite *YAMLTestSuite) TestWriteYAMLListItemNonEmptyNestedList() {
	var buf bytes.Buffer
	writeYAMLListItem(&buf, "", []interface{}{"a", "b"})
	suite.Equal("-\n  - 'a'\n  - 'b'\n", buf.String())
}

func (suite *YAMLTestSuite) TestDeterministicUUIDv7() {
	ts := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	meta := metav1.ObjectMeta{UID: types.UID("11111111-1111-4111-8111-111111111111"), CreationTimestamp: ts}

	got := deterministicUUIDv7(meta)

	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	suite.Regexp(pattern, got, "does not look like a UUIDv7")

	// Same UID + same creation time must always map to the same id (idempotency across
	// reconciles is the entire reason this function exists instead of generating randomly).
	suite.Equal(got, deterministicUUIDv7(meta))

	// A different UID must map to a different id.
	other := meta
	other.UID = types.UID("22222222-2222-4222-8222-222222222222")
	suite.NotEqual(got, deterministicUUIDv7(other))
}

func (suite *YAMLTestSuite) TestContainsAndRemoveString() {
	slice := []string{"a", "b", "c"}

	suite.True(containsString(slice, "b"))
	suite.False(containsString(slice, "z"))

	suite.Equal([]string{"a", "c"}, removeString(slice, "b"))

	// Removing an absent element is a no-op, not an error.
	suite.Equal(slice, removeString(slice, "z"))
}
