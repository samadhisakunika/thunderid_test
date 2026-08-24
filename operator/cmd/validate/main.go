package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
}

type refError struct {
	kind   string
	name   string
	field  string
	ref    string
	detail string
}

func (e refError) String() string {
	if e.detail != "" {
		return fmt.Sprintf("  [%s/%s] %s %q: %s", e.kind, e.name, e.field, e.ref, e.detail)
	}
	return fmt.Sprintf("  [%s/%s] %s %q not found", e.kind, e.name, e.field, e.ref)
}

// entityKinds is every CRD kind bound to a ThunderIDInstance via the thunderid.io/instance
// label. Their spec.config content is opaque to the operator (see CLAUDE.md's decoupling
// note) — ThunderID's own bootstrap loader validates it, and errors surface as k8s Events
// read from the pod's logs, not here. This tool only catches the one thing that IS a k8s-level
// mistake: a thunderid.io/instance label that doesn't name a real ThunderIDInstance.
var entityKinds = []string{
	"Application", "Flow", "Theme", "OrganizationUnit", "Role", "UserType",
	"User", "ResourceServer", "Group", "ServerConfig", "Translation",
}

func main() {
	allCmd := flag.NewFlagSet("all", flag.ExitOnError)
	allNS := allCmd.String("n", "default", "namespace to validate")

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load kubeconfig: %v\n", err)
		os.Exit(1)
	}
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create client: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()

	switch os.Args[1] {
	case "all":
		allCmd.Parse(os.Args[2:])
		runAll(ctx, c, *allNS)
	default:
		runFiles(ctx, c, os.Args[1:])
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  validate all [-n <namespace>]       validate all applied CRDs in the cluster")
	fmt.Fprintln(os.Stderr, "  validate <file.yaml> [file2.yaml…]  validate YAML files before applying")
}

// runAll lists every entity CR from the cluster and checks its thunderid.io/instance label.
func runAll(ctx context.Context, c client.Client, ns string) {
	var errs []refError
	for _, kind := range entityKinds {
		list, err := newListForKind(kind)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			continue
		}
		if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
			fmt.Fprintf(os.Stderr, "failed to list %ss: %v\n", kind, err)
			continue
		}
		for _, obj := range eachObject(list) {
			errs = append(errs, checkInstanceRef(ctx, c, kind, obj.GetName(), ns, obj.GetLabels()[appsv1alpha1.InstanceRefLabel])...)
		}
	}

	fmt.Printf("Validating all CRDs in namespace %q ...\n", ns)
	if len(errs) == 0 {
		fmt.Println("  OK")
	} else {
		for _, e := range errs {
			fmt.Println(e.String())
		}
		fmt.Println()
		fmt.Fprintf(os.Stderr, "%d error(s) found.\n", len(errs))
		os.Exit(1)
	}
	fmt.Println()
	fmt.Println("All references valid.")
}

// runFiles validates YAML files before applying them.
func runFiles(ctx context.Context, c client.Client, files []string) {
	totalErrors := 0
	for _, path := range files {
		fmt.Printf("Validating %s ...\n", path)
		errs := validateFile(ctx, c, path)
		if len(errs) == 0 {
			fmt.Println("  OK")
		} else {
			for _, e := range errs {
				fmt.Println(e.String())
			}
			totalErrors += len(errs)
		}
	}
	fmt.Println()
	if totalErrors > 0 {
		fmt.Fprintf(os.Stderr, "%d error(s) found. Fix these before applying.\n", totalErrors)
		os.Exit(1)
	}
	fmt.Println("All references valid.")
}

func validateFile(ctx context.Context, c client.Client, path string) []refError {
	data, err := os.ReadFile(path)
	if err != nil {
		return []refError{{kind: "file", name: path, field: "read", detail: err.Error()}}
	}

	var errs []refError
	for _, doc := range bytes.Split(data, []byte("\n---")) {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &obj); err != nil || obj.Kind == "" {
			continue
		}
		isEntity := false
		for _, k := range entityKinds {
			if obj.Kind == k {
				isEntity = true
				break
			}
		}
		if !isEntity {
			continue
		}
		ns := obj.Metadata.Namespace
		if ns == "" {
			ns = "default"
		}
		errs = append(errs, checkInstanceRef(ctx, c, obj.Kind, obj.Metadata.Name, ns, obj.Metadata.Labels[appsv1alpha1.InstanceRefLabel])...)
	}
	return errs
}

func checkInstanceRef(ctx context.Context, c client.Client, kind, name, ns, ref string) []refError {
	if ref == "" {
		return []refError{{kind: kind, name: name, field: "thunderid.io/instance", ref: "(empty)", detail: "thunderid.io/instance label is required"}}
	}
	target := &appsv1alpha1.ThunderIDInstance{}
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref}, target)
	if err == nil {
		return nil
	}
	if k8serrors.IsNotFound(err) {
		return []refError{{kind: kind, name: name, field: "thunderid.io/instance", ref: ref}}
	}
	return []refError{{kind: kind, name: name, field: "thunderid.io/instance", ref: ref, detail: err.Error()}}
}

// newListForKind returns an empty typed List object for one of entityKinds.
func newListForKind(kind string) (client.ObjectList, error) {
	switch kind {
	case "Application":
		return &appsv1alpha1.ApplicationList{}, nil
	case "Flow":
		return &appsv1alpha1.FlowList{}, nil
	case "Theme":
		return &appsv1alpha1.ThemeList{}, nil
	case "OrganizationUnit":
		return &appsv1alpha1.OrganizationUnitList{}, nil
	case "Role":
		return &appsv1alpha1.RoleList{}, nil
	case "UserType":
		return &appsv1alpha1.UserTypeList{}, nil
	case "User":
		return &appsv1alpha1.UserList{}, nil
	case "ResourceServer":
		return &appsv1alpha1.ResourceServerList{}, nil
	case "Group":
		return &appsv1alpha1.GroupList{}, nil
	case "ServerConfig":
		return &appsv1alpha1.ServerConfigList{}, nil
	case "Translation":
		return &appsv1alpha1.TranslationList{}, nil
	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
}

// eachObject extracts the individual client.Object items out of a typed List, via the
// meta.ExtractList-equivalent minimal switch needed for entityKinds.
func eachObject(list client.ObjectList) []client.Object {
	var out []client.Object
	switch l := list.(type) {
	case *appsv1alpha1.ApplicationList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.FlowList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.ThemeList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.OrganizationUnitList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.RoleList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.UserTypeList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.UserList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.ResourceServerList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.GroupList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.ServerConfigList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *appsv1alpha1.TranslationList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	}
	return out
}
