package main

import (
	"net"
	"regexp"
	"strconv"
	"strings"
)

// fence.go reads what the restored copy records about the node it was taken
// from, and refuses the copies a drill sandbox cannot start — before the
// engine is asked to, because the engine's own answer to both is to wait
// out the readiness budget without saying why.

// nodeProperties is what propertiesScript printed: the copy's own record of
// its two nodes, and the engine the sandbox carries.
type nodeProperties map[string]string

// parseProperties reads key=value lines, unescaping the backslashes Java's
// properties format writes before a colon (`127.0.0.1\:10710`, measured).
func parseProperties(out []byte) nodeProperties {
	props := nodeProperties{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" {
			continue
		}
		props[key] = strings.ReplaceAll(value, `\:`, ":")
	}
	return props
}

// spoke reports whether the output is propertiesScript's at all. Every real
// copy reaches this point with both files in place — placement refuses one
// that lacks them — so output carrying neither node's record is a stand-in
// answer (the conformance suite's simulated sandbox answers every command
// with one), and nothing here can be judged from it.
func (p nodeProperties) spoke() bool {
	return p["cn.iotdb_version"] != "" || p["dn.iotdb_version"] != ""
}

// version is major.minor.patch, compared numerically.
type version [3]int

var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?`)

// parseVersion reads the leading major.minor[.patch] of an IoTDB version.
func parseVersion(s string) (version, bool) {
	m := versionPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return version{}, false
	}
	var v version
	for i := range 3 {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return version{}, false
		}
		v[i] = n
	}
	return v, true
}

// refuseNewerCopy refuses a copy written by a newer minor line than the
// engine in the sandbox.
//
// Measured in both directions: a 1.3.7 copy starts and reads back identical
// under 2.0.11, while a 2.0.11 copy never serves under 1.3.7 — its
// ConfigNode's state machine fails at startup and the DataNode cannot find
// it — so a drill waited out its readiness budget and reported an engine
// that was not ready. Only the minor line is compared: patch releases
// within one line were not measured against each other, and refusing them
// would be a guess.
func refuseNewerCopy(props nodeProperties) *protoError {
	written, ok := parseVersion(props["cn.iotdb_version"])
	if !ok {
		return nil
	}
	engine, ok := parseVersion(props["engine.version"])
	if !ok {
		return nil
	}
	if written[0] > engine[0] || (written[0] == engine[0] && written[1] > engine[1]) {
		return protoErr("restore_failed", false,
			"the copy was written by IoTDB %s and the sandbox runs %s: a newer line's data directory "+
				"does not start on an older one (measured, 2.0 on 1.3). Drill it with an image of "+
				"%d.%d or later", props["cn.iotdb_version"], props["engine.version"], written[0], written[1])
	}
	return nil
}

// addressKeys are the settings a node records about where it listened, in
// the file that records them, and the setting of the same name the
// sandbox's configuration has to carry for the copy to start.
var addressKeys = []string{"cn.cn_internal_address", "dn.dn_internal_address", "dn.dn_rpc_address"}

// portKeys are the ports recorded beside them.
var portKeys = []string{
	"cn.cn_internal_port", "cn.cn_consensus_port",
	"dn.dn_rpc_port", "dn.dn_internal_port", "dn.dn_mpp_data_exchange_port",
	"dn.dn_schema_region_consensus_port", "dn.dn_data_region_consensus_port",
}

var (
	hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*$`)
	portPattern     = regexp.MustCompile(`^[0-9]{1,5}$`)
)

// settingName is the configuration key a recorded property maps to: the
// property's own name without the file prefix propertiesScript gave it.
func settingName(key string) string {
	_, name, _ := strings.Cut(key, ".")
	return name
}

// nodeSettings turns the copy's recorded addresses and ports into the
// sandbox's configuration, and the host names that have to resolve to
// loopback for the engine to bind them.
//
// A node configured with a host name starts in a sandbox only when the
// sandbox's configuration names the same host and that name resolves to
// loopback; either half alone gave no answer in 150 s (measured). A node
// configured with an IP address outside loopback cannot start at all: the
// sandbox has no such interface, and the ConfigNode exits with "Network is
// unreachable" (measured) — so that copy is refused here, by name, rather
// than timed out. Every value is validated, because it is written into a
// configuration file line by line.
func nodeSettings(props nodeProperties) (settings, hosts []string, perr *protoError) {
	seen := map[string]bool{}
	for _, key := range addressKeys {
		addr := props[key]
		if addr == "" {
			continue
		}
		if ip := net.ParseIP(addr); ip != nil {
			if !ip.IsLoopback() {
				return nil, nil, protoErr("restore_failed", false,
					"the copy's node was configured to listen on %s (%s), and a drill sandbox has only "+
						"loopback: the engine cannot bind that address and does not start (measured). "+
						"Address the node by a host name, which a drill maps to loopback, or restore a "+
						"copy of a node configured with 127.0.0.1", addr, settingName(key))
			}
		} else if hostnamePattern.MatchString(addr) {
			if !seen[addr] {
				seen[addr] = true
				hosts = append(hosts, addr)
			}
		} else {
			return nil, nil, protoErr("source_corrupt", false,
				"the copy records %q as %s, which is neither an IP address nor a host name",
				addr, settingName(key))
		}
		settings = append(settings, settingName(key)+"="+addr)
	}
	for _, key := range portKeys {
		port := props[key]
		if port == "" {
			continue
		}
		if !portPattern.MatchString(port) {
			return nil, nil, protoErr("source_corrupt", false,
				"the copy records %q as %s, which is not a port", port, settingName(key))
		}
		settings = append(settings, settingName(key)+"="+port)
	}
	if addr, port := props["cn.cn_internal_address"], props["cn.cn_internal_port"]; addr != "" && port != "" {
		seed := addr + ":" + port
		settings = append(settings, "cn_seed_config_node="+seed, "dn_seed_config_node="+seed)
	}
	return settings, hosts, nil
}
