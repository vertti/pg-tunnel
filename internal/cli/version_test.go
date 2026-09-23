package cli //nolint:testpackage // Exercise build metadata variants without compiling a binary for each case.

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildVersion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{name: "unavailable", want: "pg-tunnel dev"},
		{name: "test binary", info: &debug.BuildInfo{}, want: "pg-tunnel dev"},
		{name: "source archive", info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, want: "pg-tunnel dev"},
		{name: "module install", info: &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, want: "pg-tunnel v0.1.0"},
		{
			name: "release checkout",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}, Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: "abc123"}}},
			want: "pg-tunnel v0.1.0 (abc123)",
		},
		{
			name: "modified checkout",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0+dirty"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}}},
			want: "pg-tunnel v0.1.0+dirty (abc123)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, buildVersion(test.info))
		})
	}
}
