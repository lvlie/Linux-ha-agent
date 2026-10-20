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
		{ID: "lab1", Name: "lab1", Host: true, Engines: true, Temperature: true, Containers: []Container{{Key: "docker_web", Name: "web", Managed: true}}},
		{ID: "lab1_web", Name: "web", Guest: true, ViaDevice: "lab1"},
	} {
		raw, _, err := Payload(tp, Origin{Name: "linux-ha-agent", Version: "1"}, d, Features{Upgrade: true, Reboot: true, Restore: true, Schedule: true}, nil)
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

func TestPayloadRemovesVanishedComponents(t *testing.T) {
	tp := Topics{DiscoveryPrefix: "homeassistant", Base: "linux-ha-agent", HostID: "lab1"}
	d := Device{ID: "lab1_web", Name: "web", Guest: true, Engines: true, SharedKernel: true,
		Containers: []Container{{Key: "docker_a", Name: "a", Managed: true}, {Key: "docker_b", Name: "b"}}}
	_, first, err := Payload(tp, Origin{}, d, Features{Upgrade: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first["ctr_docker_b_update"] != "update" || first["ctr_docker_a_running"] != "binary_sensor" {
		t.Fatalf("components %v", first)
	}
	if _, ok := first["kernel_update"]; ok {
		t.Error("containers get no kernel update sensor")
	}
	if _, ok := first["temperature"]; ok {
		t.Error("no temperature without a sensor")
	}

	d.Containers = d.Containers[:1]
	raw, second, err := Payload(tp, Origin{}, d, Features{Upgrade: true}, first)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Cmps map[string]map[string]any `json:"cmps"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ctr_docker_b_update", "ctr_docker_b_running"} {
		if c := p.Cmps[key]; len(c) != 1 || c["p"] == nil {
			t.Errorf("%s should be a removal entry, got %v", key, c)
		}
		if _, ok := second[key]; ok {
			t.Errorf("%s must not be tracked after removal", key)
		}
	}
	// An unmanaged container has no install action.
	if _, ok := p.Cmps["ctr_docker_a_update"]["payload_install"]; !ok {
		t.Error("managed container must be installable")
	}
	_, third, _ := Payload(tp, Origin{}, Device{ID: "x", Containers: []Container{{Key: "k", Name: "k"}}}, Features{Upgrade: true}, nil)
	raw, _, _ = Payload(tp, Origin{}, Device{ID: "x", Containers: []Container{{Key: "k", Name: "k"}}}, Features{Upgrade: true}, third)
	_ = json.Unmarshal(raw, &p)
	if _, ok := p.Cmps["ctr_k_update"]["payload_install"]; ok {
		t.Error("unmanaged container must not be installable")
	}
}
