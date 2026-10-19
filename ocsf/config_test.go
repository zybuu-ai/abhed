package ocsf_test

import (
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/ocsf"
)

// record.ocsf.version accepts exactly the versions the mapper writes.
func TestConfigVersionsAreTheMappers(t *testing.T) {
	if !slices.Equal(config.OCSFVersions, ocsf.Versions) {
		t.Fatalf("config allows %v, the mapper writes %v", config.OCSFVersions, ocsf.Versions)
	}
}
