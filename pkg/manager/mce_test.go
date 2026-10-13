package manager

import (
	"strings"
	"testing"
	"unicode/utf8"

	hivev1 "github.com/openshift/hive/apis/hive/v1"
)

func TestTrimInstallLog(t *testing.T) {
	// slack in the bound for the truncation banner
	const bound = maxRetainedInstallLog + 128
	line := strings.Repeat("x", 99) + "\n"
	for _, tc := range []struct {
		name string
		log  *string
		// wantUnchanged is set when the log is expected to be returned as-is
		wantUnchanged bool
		// wantSuffix is checked against the trimmed log when set
		wantSuffix string
	}{
		{
			name: "nil log",
		},
		{
			name:          "under the limit",
			log:           ptr(strings.Repeat(line, 10)),
			wantUnchanged: true,
		},
		{
			name:       "over the limit with newlines",
			log:        ptr(strings.Repeat(line, 2*maxRetainedInstallLog/len(line)) + "level=error msg=failed\n"),
			wantSuffix: "level=error msg=failed\n",
		},
		{
			// 3-byte runes with no newlines leave the cut point in the middle
			// of a rune unless it is realigned
			name:       "over the limit with a multi-byte character at the cut",
			log:        ptr(strings.Repeat("€", maxRetainedInstallLog/3+2) + "end"),
			wantSuffix: "€€end",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provision := &hivev1.ClusterProvision{Spec: hivev1.ClusterProvisionSpec{InstallLog: tc.log}}
			var original string
			if tc.log != nil {
				original = *tc.log
			}
			trimInstallLog(provision)
			if tc.log == nil {
				if provision.Spec.InstallLog != nil {
					t.Fatalf("expected nil log to remain nil, got %q", *provision.Spec.InstallLog)
				}
				return
			}
			got := *provision.Spec.InstallLog
			if tc.wantUnchanged {
				if got != original {
					t.Errorf("expected log under the limit to be unchanged")
				}
				return
			}
			if len(got) > bound {
				t.Errorf("expected trimmed log to be at most %d bytes, got %d", bound, len(got))
			}
			if !utf8.ValidString(got) {
				t.Errorf("expected trimmed log to be valid UTF-8")
			}
			if !strings.HasPrefix(got, "[... install log truncated") {
				t.Errorf("expected trimmed log to start with a truncation banner, got %q", got[:min(len(got), 80)])
			}
			if !strings.HasSuffix(got, tc.wantSuffix) {
				t.Errorf("expected trimmed log to keep the tail %q", tc.wantSuffix)
			}
			body := got[strings.IndexByte(got, '\n')+1:]
			if !strings.HasSuffix(original, body) {
				t.Errorf("expected trimmed log to be a tail of the original")
			}
			if strings.Contains(original, "\n") && !strings.HasPrefix(body, line) {
				t.Errorf("expected trimmed log to start at a line boundary, got %q", body[:min(len(body), 80)])
			}
		})
	}
}

func ptr(s string) *string { return &s }
