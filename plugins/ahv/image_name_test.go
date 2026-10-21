package ahv

import "testing"

func TestAgentImageName(t *testing.T) {
	cases := []struct {
		name    string
		dev     bool
		version string
		want    string
	}{
		{"prod is static (version ignored)", false, "v1.2.3-5-gabc1234", "ahv-remote-console-agent.iso"},
		{"prod static even with empty version", false, "", "ahv-remote-console-agent.iso"},
		{"dev tags the git-describe version", true, "v1.2.3-5-gabc1234-dirty", "ahv-remote-console-agent-dev-v1.2.3-5-gabc1234-dirty.iso"},
		{"dev with bare \"dev\" version collapses", true, "dev", "ahv-remote-console-agent-dev.iso"},
		{"dev with empty version collapses", true, "", "ahv-remote-console-agent-dev.iso"},
		{"dev sanitizes unsafe chars", true, "1.0/feature branch", "ahv-remote-console-agent-dev-1.0-feature-branch.iso"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentImageName(tc.dev, tc.version); got != tc.want {
				t.Errorf("agentImageName(%v, %q) = %q, want %q", tc.dev, tc.version, got, tc.want)
			}
		})
	}
}

func TestSanitizeVersionTag(t *testing.T) {
	cases := map[string]string{
		"v1.2.3":              "v1.2.3",
		"v1.2.3-5-gabc-dirty": "v1.2.3-5-gabc-dirty",
		"a b/c:d":             "a-b-c-d",
		"--trim--":            "trim",
		"":                    "",
	}
	for in, want := range cases {
		if got := sanitizeVersionTag(in); got != want {
			t.Errorf("sanitizeVersionTag(%q) = %q, want %q", in, got, want)
		}
	}
}
