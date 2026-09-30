// Package kubescope says which Kubernetes resources and kinds live in a
// namespace, and which names can stand in a request path. The Kubernetes
// tools and the policy engine both read it, so a call is judged by the scope
// it runs in.
package kubescope

import (
	"regexp"
	"strings"
)

// ClusterWide is the namespace a cluster-scoped object is judged under: no
// namespace can be named "-", so no rule written for a namespace matches it.
const ClusterWide = "-"

// clusterResources are the cluster-scoped resources, by plural.
var clusterResources = map[string]bool{
	"nodes": true, "namespaces": true, "persistentvolumes": true,
	"clusterroles": true, "clusterrolebindings": true, "storageclasses": true,
	"customresourcedefinitions": true, "priorityclasses": true,
	"validatingwebhookconfigurations": true, "mutatingwebhookconfigurations": true,
	"validatingadmissionpolicies": true, "validatingadmissionpolicybindings": true,
	"ingressclasses": true, "apiservices": true, "certificatesigningrequests": true,
	"runtimeclasses": true, "csidrivers": true, "csinodes": true, "volumeattachments": true,
	"flowschemas": true, "prioritylevelconfigurations": true, "componentstatuses": true,
}

// ClusterScopedResource reports whether a resource, by its plural, is cluster-scoped.
func ClusterScopedResource(plural string) bool { return clusterResources[plural] }

var clusterKinds = map[string]bool{
	"namespace": true, "node": true, "persistentvolume": true, "clusterrole": true,
	"clusterrolebinding": true, "storageclass": true, "customresourcedefinition": true,
	"priorityclass": true, "validatingwebhookconfiguration": true,
	"mutatingwebhookconfiguration": true, "validatingadmissionpolicy": true,
	"validatingadmissionpolicybinding": true, "ingressclass": true, "apiservice": true,
	"certificatesigningrequest": true, "runtimeclass": true, "csidriver": true,
	"csinode": true, "volumeattachment": true, "flowschema": true,
	"prioritylevelconfiguration": true, "componentstatus": true,
}

var namespacedKinds = map[string]bool{
	"pod": true, "service": true, "configmap": true, "secret": true, "serviceaccount": true,
	"endpoints": true, "persistentvolumeclaim": true, "replicationcontroller": true,
	"event": true, "limitrange": true, "resourcequota": true, "podtemplate": true,
	"deployment": true, "statefulset": true, "daemonset": true, "replicaset": true,
	"controllerrevision": true, "job": true, "cronjob": true, "ingress": true,
	"networkpolicy": true, "role": true, "rolebinding": true, "poddisruptionbudget": true,
	"horizontalpodautoscaler": true, "lease": true, "endpointslice": true,
	"csistoragecapacity": true,
}

// KindScope reports whether a manifest's kind is namespaced, and whether its
// scope is known at all: a custom resource's is not.
func KindScope(kind string) (namespaced, known bool) {
	k := strings.ToLower(kind)
	switch {
	case namespacedKinds[k]:
		return true, true
	case clusterKinds[k]:
		return false, true
	}
	return false, false
}

var label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidNamespace reports whether ns can name a namespace in a request: empty
// for the default, "*" for every namespace, or a DNS-1123 label.
func ValidNamespace(ns string) bool {
	return ns == "" || ns == "*" || (len(ns) <= 63 && label.MatchString(ns))
}

// ValidName reports whether name can stand as one path segment: no separator,
// query, fragment or escape, no "..", and no whitespace or control character.
func ValidName(name string) bool {
	if name == "." || strings.Contains(name, "..") || strings.ContainsAny(name, "/?#%\\") {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029 || r == 0xa0 {
			return false
		}
	}
	return true
}

var kindRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
var apiVersionRe = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?v[0-9]+[a-z0-9]*$`)

// ValidKind reports whether kind can be made a resource's plural in a path.
func ValidKind(kind string) bool { return kindRe.MatchString(kind) }

// ValidAPIVersion reports whether apiVersion is group/version or version.
func ValidAPIVersion(v string) bool { return apiVersionRe.MatchString(v) }
