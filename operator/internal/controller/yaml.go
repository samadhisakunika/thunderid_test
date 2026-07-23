package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

// yamlSingleQuote formats s as a single-quoted YAML scalar. Go's %q escapes
// non-BMP characters (e.g. emoji) as \Uxxxxxxxx, which ThunderID's YAML
// parser doesn't support (see APP-1018). Single-quoting avoids escaping.
func yamlSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// resolveOUHandle looks up the OrganizationUnit CR whose spec.handle matches
// handle and returns its Kubernetes UID, which is also its ThunderID internal
// ID (buildOrganizationUnitYAML writes id: "<uid>"). ThunderID validates ouId
// fields by direct DB lookup at startup, while ouHandle fields trigger an HTTP
// API call that returns SSE-4030 Unauthorized before the auth layer is ready.
// Returns "" if no matching CR is found (caller should fall back to ouHandle).
func resolveOUHandle(ctx context.Context, c client.Client, ns, handle string) string {
	list := &appsv1alpha1.OrganizationUnitList{}
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return ""
	}
	for _, ou := range list.Items {
		if ou.Spec.Handle == handle {
			return string(ou.UID)
		}
	}
	return ""
}

// writeYAMLBlock writes "key:\n" followed by every line of content indented
// by 2 spaces, turning an arbitrary pasted-in multi-line YAML/JSON fragment
// into a valid nested value. Gluing such content onto the same line as its
// key (e.g. "key: <content>") breaks for block sequences ("- item" cannot
// follow "key: " on the same line) — see the Flow.Spec.Nodes bug this fixed.
func writeYAMLBlock(buf *bytes.Buffer, key, content string) {
	if content == "" {
		return
	}
	fmt.Fprintf(buf, "%s:\n", key)
	for _, line := range strings.Split(content, "\n") {
		fmt.Fprintf(buf, "  %s\n", line)
	}
}
