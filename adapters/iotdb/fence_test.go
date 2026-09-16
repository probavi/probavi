package main

import (
	"slices"
	"strings"
	"testing"
)

func TestPropertiesUnescapeWhatJavaEscapes(t *testing.T) {
	props := parseProperties([]byte("cn.config_node_list=0,127.0.0.1\\:10710\n\nnot a pair\ndn.dn_rpc_port=6667\n"))
	if props["cn.config_node_list"] != "0,127.0.0.1:10710" || props["dn.dn_rpc_port"] != "6667" || len(props) != 2 {
		t.Errorf("props = %v", props)
	}
	if props.spoke() {
		t.Error("output with no version recorded was taken as the property script's own")
	}
}

func TestANewerLineIsRefusedAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		written, engine string
		refused         bool
	}{
		{"2.0.11", "1.3.7", true},
		{"2.1.0", "2.0.11", true},
		{"3.0.0", "2.0.11", true},
		{"1.3.7", "2.0.11", false},
		{"2.0.11", "2.0.11", false},
		// Patch releases within a line were not measured against each
		// other, so they are not refused.
		{"2.0.11", "2.0.10", false},
		{"2.0.11", "", false},
		{"", "2.0.11", false},
	} {
		perr := refuseNewerCopy(nodeProperties{"cn.iotdb_version": tc.written, "engine.version": tc.engine})
		if (perr != nil) != tc.refused {
			t.Errorf("copy %q on engine %q: refused=%v, want %v", tc.written, tc.engine, perr != nil, tc.refused)
		}
	}
}

func TestNodeSettingsCarryTheCopysAddressesAndRefuseTheUnbindable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		props     nodeProperties
		hosts     []string
		settings  []string
		code      string
		mentioned string
	}{
		{
			name:     "loopback",
			props:    nodeProperties{"cn.cn_internal_address": "127.0.0.1", "cn.cn_internal_port": "10710", "dn.dn_rpc_address": "127.0.0.2"},
			settings: []string{"cn_internal_address=127.0.0.1", "dn_rpc_address=127.0.0.2", "cn_internal_port=10710", "cn_seed_config_node=127.0.0.1:10710", "dn_seed_config_node=127.0.0.1:10710"},
		},
		{
			name:     "IPv6 loopback",
			props:    nodeProperties{"dn.dn_internal_address": "::1"},
			settings: []string{"dn_internal_address=::1"},
		},
		{
			name:     "a host name, mapped once",
			props:    nodeProperties{"cn.cn_internal_address": "iotdb-prod-1", "dn.dn_internal_address": "iotdb-prod-1", "dn.dn_rpc_address": "rpc.example.internal"},
			hosts:    []string{"iotdb-prod-1", "rpc.example.internal"},
			settings: []string{"cn_internal_address=iotdb-prod-1", "dn_internal_address=iotdb-prod-1", "dn_rpc_address=rpc.example.internal"},
		},
		{name: "a routable address", props: nodeProperties{"dn.dn_internal_address": "10.9.8.7"}, code: "restore_failed", mentioned: "10.9.8.7"},
		{name: "a routable IPv6 address", props: nodeProperties{"cn.cn_internal_address": "fd00::7"}, code: "restore_failed", mentioned: "fd00::7"},
		// A value written into a configuration file line by line cannot be
		// allowed to carry a line of its own.
		{name: "an address carrying a newline", props: nodeProperties{"cn.cn_internal_address": "a\nenable_auth=false"}, code: "source_corrupt", mentioned: "neither an IP address nor a host name"},
		{name: "a port that is not one", props: nodeProperties{"dn.dn_rpc_port": "6667\nx=y"}, code: "source_corrupt", mentioned: "not a port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, hosts, perr := nodeSettings(tc.props)
			if tc.code != "" {
				if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.mentioned) {
					t.Errorf("got %+v, want %s mentioning %q", perr, tc.code, tc.mentioned)
				}
				return
			}
			if perr != nil {
				t.Fatalf("refused: %+v", perr)
			}
			if !slices.Equal(hosts, tc.hosts) {
				t.Errorf("hosts = %v, want %v", hosts, tc.hosts)
			}
			if !slices.Equal(settings, tc.settings) {
				t.Errorf("settings = %v, want %v", settings, tc.settings)
			}
		})
	}
}
