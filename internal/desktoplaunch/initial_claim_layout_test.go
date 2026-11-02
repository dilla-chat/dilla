package desktoplaunch

import (
	"strings"
	"testing"
)

func initialLayoutFixture(t *testing.T) *InitialCopyLayout {
	t.Helper()
	raw := manifestBytes([]map[string]any{fileEntry("image.exe", "program"), fileEntry("resources/"+strings.Repeat("a", 90)+"/"+strings.Repeat("b", 90)+"/image.exe", "nested")})
	layout, err := NewInitialCopyLayout(raw, digest(raw), stableContext, InitialBootstrapFiles{ProductName: "Fixture", LauncherSize: 3, LauncherSHA512: digest([]byte("go!")), UninstallerSize: 5, UninstallerSHA512: digest([]byte("unins"))})
	if err != nil {
		t.Fatal(err)
	}
	return layout
}
func TestInitialCopyLayoutBindsExactMetadataAndManifestRelativePaths(t *testing.T) {
	layout := initialLayoutFixture(t)
	for path := range layout.files {
		if _, ok := layout.expectedFile(path); !ok {
			t.Fatalf("missing bound path %q", path)
		}
	}
	if len(layout.files) != 5 {
		t.Fatal("omitted complete package entries")
	}
	for _, path := range []string{"../outside", "manifests/" + strings.Repeat("b", 128) + ".json", "versions/v-" + strings.Repeat("c", 32) + "/image.exe", "Versions/" + layout.treeID + "/image.exe", "versions/" + layout.treeID + "/../outside", "versions/" + layout.treeID + "/image.exe:secret"} {
		if _, ok := layout.expectedFile(path); ok {
			t.Fatalf("accepted unbound path %q", path)
		}
	}
	if len(layout.manifestName) != 133 {
		t.Fatal("fixed digest manifest component was not retained")
	}
}
