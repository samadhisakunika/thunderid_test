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
	"testing"

	"github.com/stretchr/testify/suite"
	"sigs.k8s.io/yaml"
)

type YAMLTestSuite struct {
	suite.Suite
}

func TestYAMLTestSuite(t *testing.T) {
	suite.Run(t, new(YAMLTestSuite))
}

func (suite *YAMLTestSuite) TestRenderResourceYAML() {
	got := renderResourceYAML(map[string]any{
		"name": "console",
		"port": float64(8090),
		"tags": []any{"a", "b"},
		"nested": map[string]any{
			"inner": true,
		},
		"empty_map":  map[string]any{},
		"empty_list": []any{},
	})

	// Keys are written sorted, regardless of map iteration order.
	want := "empty_list: []\n" +
		"empty_map: {}\n" +
		"name: console\n" +
		"nested:\n" +
		"  inner: true\n" +
		"port: 8090\n" +
		"tags:\n" +
		"  - a\n" +
		"  - b\n"
	suite.Equal(want, got)
}

func (suite *YAMLTestSuite) TestRenderResourceYAMLEmojiRoundTrips() {
	// The whole point of switching to a real YAML library (see yaml.go's doc comment, APP-1018)
	// only worked once it was confirmed live that ThunderID's own parser can decode a non-BMP
	// rune (emoji) scalar today; this checks the operator's own render+parse round trip stays
	// exact regardless.
	in := "emoji:👨‍💻 and 'quotes'"
	doc := renderResourceYAML(map[string]any{"key": in})

	var parsed map[string]string
	suite.Require().NoError(yaml.Unmarshal([]byte(doc), &parsed))
	suite.Equal(in, parsed["key"])
}

func (suite *YAMLTestSuite) TestRenderResourceYAMLScalarTypes() {
	got := renderResourceYAML(map[string]any{
		"str":             "abc",
		"boolTrue":        true,
		"boolFalse":       false,
		"nilVal":          nil,
		"wholeFloat":      float64(42),
		"negativeFloat":   float64(-7),
		"fractionalFloat": float64(3.5),
	})

	want := "boolFalse: false\n" +
		"boolTrue: true\n" +
		"fractionalFloat: 3.5\n" +
		"negativeFloat: -7\n" +
		"nilVal: null\n" +
		"str: abc\n" +
		"wholeFloat: 42\n"
	suite.Equal(want, got)
}

func (suite *YAMLTestSuite) TestRenderResourceYAMLAmbiguousStringsRoundTrip() {
	// Strings that look like other YAML types must still decode back to strings.
	got := renderResourceYAML(map[string]any{
		"looksBool": "true",
		"looksNum":  "123",
		"looksNull": "null",
	})

	var parsed map[string]any
	suite.Require().NoError(yaml.Unmarshal([]byte(got), &parsed))
	suite.Equal("true", parsed["looksBool"])
	suite.Equal("123", parsed["looksNum"])
	suite.Equal("null", parsed["looksNull"])
}

func (suite *YAMLTestSuite) TestContainsAndRemoveString() {
	slice := []string{"a", "b", "c"}

	suite.True(containsString(slice, "b"))
	suite.False(containsString(slice, "z"))

	suite.Equal([]string{"a", "c"}, removeString(slice, "b"))

	// Removing an absent element is a no-op, not an error.
	suite.Equal(slice, removeString(slice, "z"))
}
