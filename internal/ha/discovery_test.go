package ha

import (
	"encoding/json"
	"testing"
)

func TestSlug(t *testing.T) {
	cases := map[string][]string{
		"lab1":            {"Lab1"},
		"lab1_web_server": {"lab1", "web.server"},
		"my_host":         {"--My Host!--"},
		"lab1_dev_fed":    {"lab1", "dev", "fed"},
	}
	for want, in := range cases {
		if got := Slug(in...); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTopics(t *testing.T) {
	tp := Topics{DiscoveryPrefix: "homeassistant", Base: "linux-ha-agent", HostID: "lab1"}
	if tp.Config("lab1_web") != "homeassistant/device/lab1/lab1_web/config" ||
		tp.Command("lab1_web") != "linux-ha-agent/lab1/lab1_web/cmd" ||
		tp.CommandFilter() != "linux-ha-agent/lab1/+/cmd" ||
		tp.Availability() != "linux-ha-agent/lab1/status" {
		t.Fatalf("unexpected topics %+v", tp)
	}
}

func TestPayloadIsValidDeviceDiscovery(t *testing.T) {
	tp := Topics{DiscoveryPrefix: "homeassistant", Base: "linux-ha-agent", HostID: "lab1"}
	for _, d := range []Device{
		{ID: "lab1", Name: "lab1", Host: true, Containers: true},
		{ID: "lab1_web", Name: "web", Guest: true, ViaDevice: "lab1"},
	} {
		raw, err := Payload(tp, Origin{Name: "linux-ha-agent", Version: "1"}, d, Features{Upgrade: true, Reboot: true, Restore: true, Schedule: true})
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Dev  map[string]any            `json:"dev"`
			O    map[string]any            `json:"o"`
			Cmps map[string]map[string]any `json:"cmps"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if p.O["name"] != "linux-ha-agent" || p.Dev["name"] != d.Name {
			t.Errorf("%s: bad device/origin %v %v", d.ID, p.Dev, p.O)
		}
		ids := map[string]bool{}
		for key, c := range p.Cmps {
			uid, _ := c["unique_id"].(string)
			if c["p"] == nil || c["name"] == nil || uid == "" || ids[uid] {
				t.Errorf("%s/%s: invalid component %v", d.ID, key, c)
			}
			ids[uid] = true
			if c["availability_topic"] != "linux-ha-agent/lab1/status" {
				t.Errorf("%s/%s: no availability", d.ID, key)
			}
			_, hasState := c["state_topic"]
			if (c["p"] == "button") == hasState {
				t.Errorf("%s/%s: buttons must have no state topic, others must", d.ID, key)
			}
		}
		_, restore := p.Cmps["restore_snapshot"]
		_, upgradeAll := p.Cmps["upgrade_all"]
		if restore != d.Guest || upgradeAll != d.Host {
			t.Errorf("%s: restore=%v upgrade_all=%v", d.ID, restore, upgradeAll)
		}
		if d.Guest && p.Cmps["reboot"]["name"] != "Restart" {
			t.Errorf("guest reboot button should be called Restart")
		}
	}
}
