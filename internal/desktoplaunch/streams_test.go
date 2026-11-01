package desktoplaunch

import "testing"

// Directory-entry enumeration and hashing only the unnamed stream cannot detect
// named NTFS $DATA streams. The content policy must reject every such stream.
func TestDataStreamPolicyRejectsContentOutsideDefaultFileStream(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory bool
		streams   []string
		valid     bool
	}{
		{"default file", false, []string{"::$DATA"}, true},
		{"empty directory", true, nil, true},
		{"file ADS", false, []string{"::$DATA", ":payload:$DATA"}, false},
		{"directory ADS", true, []string{":payload:$DATA"}, false},
		{"directory default", true, []string{"::$DATA"}, false},
		{"missing file default", false, nil, false},
		{"duplicate default", false, []string{"::$DATA", "::$DATA"}, false},
		{"named DATA", false, []string{"::$DATA", ":$DATA:$DATA"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := dataStreamPolicy{directory: test.directory}
			var err error
			for _, name := range test.streams {
				if err = policy.add(name); err != nil {
					break
				}
			}
			if err == nil {
				err = policy.finish()
			}
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
		})
	}
}
