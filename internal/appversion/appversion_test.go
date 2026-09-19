package appversion

import (
	"runtime/debug"
	"testing"
)

func TestDescribe(t *testing.T) {
	const rev = "0123456789abcdef0123456789abcdef01234567"
	vcs := func(revision, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{
			{Key: "-buildmode", Value: "exe"},
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: revision},
			{Key: "vcs.time", Value: "2026-09-19T00:00:00Z"},
			{Key: "vcs.modified", Value: modified},
		}
	}

	tests := []struct {
		name     string
		version  string
		settings []debug.BuildSetting
		want     string
	}{
		{"a release version wins over the revision", "v0.1.0", vcs(rev, "false"), "v0.1.0"},
		{"a release version wins over a dirty tree", "v0.1.0", vcs(rev, "true"), "v0.1.0"},
		{"a release version needs no build info", "v0.1.0", nil, "v0.1.0"},
		{"a clean revision is cut to 12 characters", "", vcs(rev, "false"), "dev-0123456789ab"},
		{"a modified tree says so", "", vcs(rev, "true"), "dev-0123456789ab-dirty"},
		{"only the word true means modified", "", vcs(rev, "TRUE"), "dev-0123456789ab"},
		{"exactly 12 characters are kept whole", "", vcs("0123456789ab", "false"), "dev-0123456789ab"},
		{"13 characters lose one", "", vcs("0123456789abc", "false"), "dev-0123456789ab"},
		{"a short revision is kept whole", "", vcs("abc123", "false"), "dev-abc123"},
		{"a short modified revision", "", vcs("abc123", "true"), "dev-abc123-dirty"},
		{"no build info at all", "", nil, "dev"},
		{"an empty revision", "", vcs("", "false"), "dev"},
		{"modified without a revision is still just dev", "", vcs("", "true"), "dev"},
		{"settings with no vcs keys, as under go test", "", []debug.BuildSetting{{Key: "-buildmode", Value: "exe"}}, "dev"},
		{
			"modified listed before the revision", "",
			[]debug.BuildSetting{{Key: "vcs.modified", Value: "true"}, {Key: "vcs.revision", Value: rev}},
			"dev-0123456789ab-dirty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describe(tt.version, tt.settings); got != tt.want {
				t.Errorf("describe(%q, ...) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestStringUsesTheLinkerVersion(t *testing.T) {
	// String is describe over the real inputs. The one thing describe cannot show is that the
	// variable the linker sets is the one String reads.
	old := Version
	t.Cleanup(func() { Version = old })

	Version = "v9.9.9-test"
	if got := String(); got != "v9.9.9-test" {
		t.Errorf("String() = %q with Version set, want the version", got)
	}

	Version = ""
	if got, want := String(), describe("", buildSettings()); got != want {
		t.Errorf("String() = %q, want %q from the build settings", got, want)
	}
	if got := String(); got == "" {
		t.Error("String() is empty for a development build")
	}
}
