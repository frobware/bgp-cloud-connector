package gcp

import (
	"fmt"
	"regexp"
	"strings"
)

var providerIDRe = regexp.MustCompile(`^gce://(?P<project>[^/]+)/(?P<zone>[^/]+)/(?P<name>.+)$`)

// Instance is a GCE instance identified from a Kubernetes provider ID.
type Instance struct {
	Project  string
	Zone     string
	Name     string
	SelfLink string
}

// ParseProviderID extracts GCE instance identity from a Node or Machine
// spec.providerID of the form gce://PROJECT/ZONE/NAME.
func ParseProviderID(providerID string) (Instance, error) {
	m := providerIDRe.FindStringSubmatch(providerID)
	if m == nil {
		return Instance{}, fmt.Errorf("not a GCE provider ID: %q", providerID)
	}

	var inst Instance
	for i, name := range providerIDRe.SubexpNames() {
		switch name {
		case "project":
			inst.Project = m[i]
		case "zone":
			inst.Zone = m[i]
		case "name":
			inst.Name = m[i]
		}
	}
	inst.SelfLink = SelfLink(inst.Project, inst.Zone, inst.Name)
	return inst, nil
}

// SelfLink returns the fully qualified GCE instance URL, which is how both
// Cloud Router peers and NCC spokes refer to an instance.
func SelfLink(project, zone, name string) string {
	return fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/zones/%s/instances/%s", project, zone, name)
}

// shortZone reduces a zone URL or path to its bare name.
func shortZone(zone string) string {
	if i := strings.LastIndex(zone, "/"); i >= 0 && i+1 < len(zone) {
		return zone[i+1:]
	}
	return zone
}
