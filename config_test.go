package main

import "testing"

func TestParseConfigDefaults(t *testing.T) {
	cfg, warnings, err := parseConfig(nil, `C:\Users\me`)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if cfg.DefaultCwd != `C:\Users\me` {
		t.Errorf("DefaultCwd = %q", cfg.DefaultCwd)
	}
	if cfg.LocalLabel != "Windows" || cfg.LocalColor != "#3b82f6" {
		t.Errorf("local defaults = %q %q", cfg.LocalLabel, cfg.LocalColor)
	}
	if cfg.Hosts == nil || len(cfg.Hosts) != 0 {
		t.Errorf("Hosts = %#v, want empty non-nil slice", cfg.Hosts)
	}
}

func TestParseConfigHosts(t *testing.T) {
	raw := `{"default_cwd":"C:\\Desktop","hosts":[
		{"id":"mac","label":"Mac","ssh":"mac","default_cwd":"~/Desktop/My Projects","color":"#f97316"},
		{"id":"box"}
	]}`
	cfg, warnings, err := parseConfig([]byte(raw), `C:\Users\me`)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(cfg.Hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(cfg.Hosts))
	}
	mac := cfg.Hosts[0]
	if mac.ID != "mac" || mac.SSH != "mac" || mac.DefaultCwd != "~/Desktop/My Projects" {
		t.Errorf("mac = %+v", mac)
	}
	box := cfg.Hosts[1]
	if box.SSH != "box" || box.Label != "box" || box.Color != "#f97316" || box.DefaultCwd != "~" {
		t.Errorf("box defaults = %+v", box)
	}
}

func TestParseConfigRejectsBadHosts(t *testing.T) {
	raw := `{"hosts":[{"id":"Bad ID"},{"id":"mac"},{"id":"mac"}]}`
	cfg, warnings, err := parseConfig([]byte(raw), "home")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hosts) != 1 || cfg.Hosts[0].ID != "mac" {
		t.Errorf("hosts = %+v, want only mac", cfg.Hosts)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %v, want 2", warnings)
	}
}

func TestParseConfigInvalidJSON(t *testing.T) {
	if _, _, err := parseConfig([]byte("{nope"), "home"); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseConfigSSHValidation(t *testing.T) {
	raw := `{"hosts":[{"id":"a","ssh":"-oProxyCommand=x"},{"id":"b","ssh":"has space"},{"id":"-x"},{"id":"ok","ssh":"user@host"}]}`
	cfg, warnings, err := parseConfig([]byte(raw), "home")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hosts) != 1 || cfg.Hosts[0].ID != "ok" {
		t.Errorf("hosts = %+v, want only ok", cfg.Hosts)
	}
	if len(warnings) != 3 {
		t.Errorf("warnings = %v, want 3", warnings)
	}
}

func TestParseConfigColorValidation(t *testing.T) {
	raw := `{"local_color":"red","hosts":[{"id":"a","color":"javascript:x"},{"id":"b","color":"#abc"}]}`
	cfg, warnings, err := parseConfig([]byte(raw), "home")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalColor != defaultLocalColor {
		t.Errorf("LocalColor = %q", cfg.LocalColor)
	}
	if len(cfg.Hosts) != 2 || cfg.Hosts[0].Color != defaultHostColor || cfg.Hosts[1].Color != "#abc" {
		t.Errorf("hosts = %+v", cfg.Hosts)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %v, want 2", warnings)
	}
}

func TestColorPatternLengths(t *testing.T) {
	cases := map[string]bool{
		"#abc": true, "#abcd": true, "#a1b2c3": true, "#a1b2c3d4": true,
		"#12345": false, "#1234567": false, "#12": false, "#123456789": false, "#ggg": false,
	}
	for color, want := range cases {
		if got := colorPattern.MatchString(color); got != want {
			t.Errorf("colorPattern(%q) = %v, want %v", color, got, want)
		}
	}
}
