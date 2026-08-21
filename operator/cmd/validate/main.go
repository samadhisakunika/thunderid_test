package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

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

type typeMeta struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
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

// baseHandles caches handles seeded from the thunderid-resources ConfigMap.
type baseHandles struct {
	flows  map[string]bool
	ous    map[string]bool
	themes map[string]bool
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

// runAll lists every CR from the cluster and validates handle references.
func runAll(ctx context.Context, c client.Client, ns string) {
	baseCache := map[string]baseHandles{}
	var errs []refError

	// Applications
	var apps appsv1alpha1.ApplicationList
	if err := c.List(ctx, &apps, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to list Applications: %v\n", err)
	} else {
		for _, obj := range apps.Items {
			obj.Namespace = ns
			base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
			errs = append(errs, checkApplication(ctx, c, obj, base)...)
		}
	}

	// Flows
	var flows appsv1alpha1.FlowList
	if err := c.List(ctx, &flows, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to list Flows: %v\n", err)
	} else {
		for _, obj := range flows.Items {
			errs = append(errs, checkInstanceRef(ctx, c, "Flow", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
		}
	}

	// Themes
	var themes appsv1alpha1.ThemeList
	if err := c.List(ctx, &themes, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to list Themes: %v\n", err)
	} else {
		for _, obj := range themes.Items {
			errs = append(errs, checkInstanceRef(ctx, c, "Theme", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
		}
	}

	// OrganizationUnits
	var ous appsv1alpha1.OrganizationUnitList
	if err := c.List(ctx, &ous, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to list OrganizationUnits: %v\n", err)
	} else {
		for _, obj := range ous.Items {
			base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
			errs = append(errs, checkInstanceRef(ctx, c, "OrganizationUnit", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
			if obj.Spec.Parent != "" {
				errs = append(errs, checkOUHandle(ctx, c, "OrganizationUnit", obj.Name, ns, "parentHandle", obj.Spec.Parent, base)...)
			}
		}
	}

	// ResourceServers
	var rss appsv1alpha1.ResourceServerList
	if err := c.List(ctx, &rss, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to list ResourceServers: %v\n", err)
	} else {
		for _, obj := range rss.Items {
			base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
			errs = append(errs, checkInstanceRef(ctx, c, "ResourceServer", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
			if obj.Spec.OUHandle != "" {
				errs = append(errs, checkOUHandle(ctx, c, "ResourceServer", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
			}
		}
	}

	// Roles
	var roles appsv1alpha1.RoleList
	if err := c.List(ctx, &roles, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to list Roles: %v\n", err)
	} else {
		for _, obj := range roles.Items {
			base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
			errs = append(errs, checkInstanceRef(ctx, c, "Role", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
			if obj.Spec.OUHandle != "" {
				errs = append(errs, checkOUHandle(ctx, c, "Role", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
			}
		}
	}

	// UserTypes
	var uts appsv1alpha1.UserTypeList
	if err := c.List(ctx, &uts, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to list UserTypes: %v\n", err)
	} else {
		for _, obj := range uts.Items {
			base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
			errs = append(errs, checkInstanceRef(ctx, c, "UserType", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
			if obj.Spec.OUHandle != "" {
				errs = append(errs, checkOUHandle(ctx, c, "UserType", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
			}
		}
	}

	// Users
	var users appsv1alpha1.UserList
	if err := c.List(ctx, &users, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to list Users: %v\n", err)
	} else {
		for _, obj := range users.Items {
			base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
			errs = append(errs, checkInstanceRef(ctx, c, "User", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
			if obj.Spec.OUHandle != "" {
				errs = append(errs, checkOUHandle(ctx, c, "User", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
			}
		}
	}

	// Groups
	var groups appsv1alpha1.GroupList
	if err := c.List(ctx, &groups, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to list Groups: %v\n", err)
	} else {
		for _, obj := range groups.Items {
			base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
			errs = append(errs, checkInstanceRef(ctx, c, "Group", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
			if obj.Spec.OUHandle != "" {
				errs = append(errs, checkOUHandle(ctx, c, "Group", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
			}
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

	// base handle cache keyed by "ns/instanceRef", loaded lazily per instance
	baseCache := map[string]baseHandles{}

	var errs []refError
	for _, doc := range bytes.Split(data, []byte("\n---")) {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		var tm typeMeta
		if err := yaml.Unmarshal(doc, &tm); err != nil || tm.Kind == "" {
			continue
		}
		ns := tm.Metadata.Namespace
		if ns == "" {
			ns = "default"
		}

		switch tm.Kind {
		case "Application":
			var obj appsv1alpha1.Application
			if err := yaml.Unmarshal(doc, &obj); err == nil {
				obj.Namespace = ns
				base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
				errs = append(errs, checkApplication(ctx, c, obj, base)...)
			}
		case "Flow":
			var obj appsv1alpha1.Flow
			if err := yaml.Unmarshal(doc, &obj); err == nil {
				errs = append(errs, checkInstanceRef(ctx, c, "Flow", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
			}
		case "Theme":
			var obj appsv1alpha1.Theme
			if err := yaml.Unmarshal(doc, &obj); err == nil {
				errs = append(errs, checkInstanceRef(ctx, c, "Theme", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
			}
		case "OrganizationUnit":
			var obj appsv1alpha1.OrganizationUnit
			if err := yaml.Unmarshal(doc, &obj); err == nil {
				base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
				errs = append(errs, checkInstanceRef(ctx, c, "OrganizationUnit", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
				if obj.Spec.Parent != "" {
					errs = append(errs, checkOUHandle(ctx, c, "OrganizationUnit", obj.Name, ns, "parentHandle", obj.Spec.Parent, base)...)
				}
			}
		case "ResourceServer":
			var obj appsv1alpha1.ResourceServer
			if err := yaml.Unmarshal(doc, &obj); err == nil {
				base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
				errs = append(errs, checkInstanceRef(ctx, c, "ResourceServer", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
				if obj.Spec.OUHandle != "" {
					errs = append(errs, checkOUHandle(ctx, c, "ResourceServer", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
				}
			}
		case "Role":
			var obj appsv1alpha1.Role
			if err := yaml.Unmarshal(doc, &obj); err == nil {
				base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
				errs = append(errs, checkInstanceRef(ctx, c, "Role", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
				if obj.Spec.OUHandle != "" {
					errs = append(errs, checkOUHandle(ctx, c, "Role", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
				}
			}
		case "UserType":
			var obj appsv1alpha1.UserType
			if err := yaml.Unmarshal(doc, &obj); err == nil {
				base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
				errs = append(errs, checkInstanceRef(ctx, c, "UserType", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
				if obj.Spec.OUHandle != "" {
					errs = append(errs, checkOUHandle(ctx, c, "UserType", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
				}
			}
		case "User":
			var obj appsv1alpha1.User
			if err := yaml.Unmarshal(doc, &obj); err == nil {
				base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
				errs = append(errs, checkInstanceRef(ctx, c, "User", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
				if obj.Spec.OUHandle != "" {
					errs = append(errs, checkOUHandle(ctx, c, "User", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
				}
			}
		case "Group":
			var obj appsv1alpha1.Group
			if err := yaml.Unmarshal(doc, &obj); err == nil {
				base := getBaseHandles(ctx, c, baseCache, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])
				errs = append(errs, checkInstanceRef(ctx, c, "Group", obj.Name, ns, obj.Labels[appsv1alpha1.InstanceRefLabel])...)
				if obj.Spec.OUHandle != "" {
					errs = append(errs, checkOUHandle(ctx, c, "Group", obj.Name, ns, "ouHandle", obj.Spec.OUHandle, base)...)
				}
			}
		}
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

func checkApplication(ctx context.Context, c client.Client, app appsv1alpha1.Application, base baseHandles) []refError {
	ns := app.Namespace
	var errs []refError
	errs = append(errs, checkInstanceRef(ctx, c, "Application", app.Name, ns, app.Labels[appsv1alpha1.InstanceRefLabel])...)
	if app.Spec.OUHandle != "" {
		errs = append(errs, checkOUHandle(ctx, c, "Application", app.Name, ns, "ouHandle", app.Spec.OUHandle, base)...)
	}
	if app.Spec.AuthFlowHandle != "" {
		errs = append(errs, checkFlowHandle(ctx, c, "Application", app.Name, ns, "authFlowHandle", app.Spec.AuthFlowHandle, base)...)
	}
	if app.Spec.RegistrationFlowHandle != "" {
		errs = append(errs, checkFlowHandle(ctx, c, "Application", app.Name, ns, "registrationFlowHandle", app.Spec.RegistrationFlowHandle, base)...)
	}
	if app.Spec.RecoveryFlowHandle != "" {
		errs = append(errs, checkFlowHandle(ctx, c, "Application", app.Name, ns, "recoveryFlowHandle", app.Spec.RecoveryFlowHandle, base)...)
	}
	if app.Spec.ThemeHandle != "" {
		errs = append(errs, checkThemeHandle(ctx, c, "Application", app.Name, ns, "themeHandle", app.Spec.ThemeHandle, base)...)
	}
	return errs
}

func checkFlowHandle(ctx context.Context, c client.Client, kind, name, ns, field, handle string, base baseHandles) []refError {
	if base.flows[handle] {
		return nil
	}
	list := &appsv1alpha1.FlowList{}
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return []refError{{kind: kind, name: name, field: field, ref: handle, detail: err.Error()}}
	}
	for _, f := range list.Items {
		if f.Spec.Handle == handle {
			return nil
		}
	}
	return []refError{{kind: kind, name: name, field: field, ref: handle}}
}

func checkOUHandle(ctx context.Context, c client.Client, kind, name, ns, field, handle string, base baseHandles) []refError {
	if base.ous[handle] {
		return nil
	}
	list := &appsv1alpha1.OrganizationUnitList{}
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return []refError{{kind: kind, name: name, field: field, ref: handle, detail: err.Error()}}
	}
	for _, ou := range list.Items {
		if ou.Spec.Handle == handle {
			return nil
		}
	}
	return []refError{{kind: kind, name: name, field: field, ref: handle}}
}

func checkThemeHandle(ctx context.Context, c client.Client, kind, name, ns, field, handle string, base baseHandles) []refError {
	if base.themes[handle] {
		return nil
	}
	// theme handle == CR metadata.name (set by buildThemeYAML as t.Name)
	target := &appsv1alpha1.Theme{}
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: handle}, target)
	if err == nil {
		return nil
	}
	if k8serrors.IsNotFound(err) {
		return []refError{{kind: kind, name: name, field: field, ref: handle}}
	}
	return []refError{{kind: kind, name: name, field: field, ref: handle, detail: err.Error()}}
}

// getBaseHandles returns cached base handles for the given instance, loading lazily.
func getBaseHandles(ctx context.Context, c client.Client, cache map[string]baseHandles, ns, instanceRef string) baseHandles {
	key := ns + "/" + instanceRef
	if bh, ok := cache[key]; ok {
		return bh
	}
	bh := loadBaseHandles(ctx, c, ns, instanceRef)
	cache[key] = bh
	return bh
}

func loadBaseHandles(ctx context.Context, c client.Client, ns, instanceRef string) baseHandles {
	empty := baseHandles{flows: map[string]bool{}, ous: map[string]bool{}, themes: map[string]bool{}}
	if instanceRef == "" {
		return empty
	}
	instance := &appsv1alpha1.ThunderIDInstance{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: instanceRef}, instance); err != nil {
		return empty
	}
	if instance.Spec.ResourcesConfigMap == "" {
		return empty
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: instance.Spec.ResourcesConfigMap}, cm); err != nil {
		return empty
	}
	return parseBaseHandles(cm.Data["resources.yaml"])
}

// parseBaseHandles reads YAML documents from the thunderid-resources ConfigMap.
// Each document starts with a comment "# resource_type: <type>" header that identifies what kind of resource the document describes.
func parseBaseHandles(content string) baseHandles {
	bh := baseHandles{flows: map[string]bool{}, ous: map[string]bool{}, themes: map[string]bool{}}
	var currentType string
	for _, doc := range bytes.Split([]byte(content), []byte("\n---")) {
		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}
		for _, line := range bytes.Split(doc, []byte("\n")) {
			l := strings.TrimSpace(string(line))
			if strings.HasPrefix(l, "# resource_type:") {
				currentType = strings.TrimSpace(strings.TrimPrefix(l, "# resource_type:"))
				break
			}
		}
		var obj map[string]interface{}
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			continue
		}
		handle, _ := obj["handle"].(string)
		if handle == "" {
			continue
		}
		switch currentType {
		case "flow":
			bh.flows[handle] = true
		case "organization_unit":
			bh.ous[handle] = true
		case "theme":
			bh.themes[handle] = true
		}
	}
	return bh
}
