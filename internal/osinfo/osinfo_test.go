package osinfo

import (
	"context"
	"testing"

	"github.com/lvlie/linux-ha-agent/internal/runner"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		content string
		id      string
		family  Family
		pretty  string
		version string
	}{
		{
			name: "ubuntu",
			content: `PRETTY_NAME="Ubuntu 24.04.3 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
ID=ubuntu
ID_LIKE=debian
`,
			id: "ubuntu", family: FamilyDebian, pretty: "Ubuntu 24.04.3 LTS", version: "24.04",
		},
		{
			name:    "debian",
			content: "NAME=\"Debian GNU/Linux\"\nVERSION_ID=\"13\"\nID=debian\n",
			id:      "debian", family: FamilyDebian, pretty: "Debian GNU/Linux 13", version: "13",
		},
		{
			name:    "fedora",
			content: "NAME=\"Fedora Linux\"\nVERSION_ID=43\nID=fedora\nPRETTY_NAME=\"Fedora Linux 43 (Container Image)\"\n",
			id:      "fedora", family: FamilyRHEL, pretty: "Fedora Linux 43 (Container Image)", version: "43",
		},
		{
			name:    "rocky",
			content: "NAME=\"Rocky Linux\"\nID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"10.0\"\n",
			id:      "rocky", family: FamilyRHEL, pretty: "Rocky Linux 10.0", version: "10.0",
		},
		{
			name:    "nixos",
			content: "NAME=NixOS\nID=nixos\nVERSION_ID=\"25.11\"\nPRETTY_NAME=\"NixOS 25.11 (Xantusia)\"\n",
			id:      "nixos", family: FamilyNixOS, pretty: "NixOS 25.11 (Xantusia)", version: "25.11",
		},
		{
			name:    "alpine",
			content: "NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.22.0\n# comment\n",
			id:      "alpine", family: FamilyUnknown, pretty: "Alpine Linux 3.22.0", version: "3.22.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := Parse(tt.content)
			if info.ID != tt.id || info.Family != tt.family || info.PrettyName != tt.pretty || info.Version != tt.version {
				t.Fatalf("got %+v", info)
			}
		})
	}
}

func TestDetectError(t *testing.T) {
	f := (&runner.Fake{}).On("cat /etc/os-release", runner.Result{ExitCode: 1, Stderr: "no such file"})
	info, err := Detect(context.Background(), f)
	if err == nil || info.Family != FamilyUnknown {
		t.Fatalf("expected error and unknown family, got %+v %v", info, err)
	}
}
