package dockerproxy

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// rule checks one field's value.
type rule func(json.RawMessage) error

// allow accepts any value of a field.
func allow(json.RawMessage) error { return nil }

// oneOf accepts the given strings (and an absent value).
func oneOf(field string, values ...string) rule {
	return func(v json.RawMessage) error {
		if isZero(v) {
			return nil
		}
		var s string
		if json.Unmarshal(v, &s) != nil || !slices.Contains(values, s) {
			return deny("%s %s is not allowed", field, v)
		}
		return nil
	}
}

// null accepts only no value: an empty list differs from none (no masked
// paths of /proc at all).
func null(field string) rule {
	return func(v json.RawMessage) error {
		if !isNull(v) {
			return deny("%s may not be set", field)
		}
		return nil
	}
}

// object checks a field holding an object with check (an object of empty
// fields is checked too: an empty list may mean something).
func object(check func(map[string]json.RawMessage) error) rule {
	return func(v json.RawMessage) error {
		if isNull(v) {
			return nil
		}
		var o map[string]json.RawMessage
		if err := json.Unmarshal(v, &o); err != nil {
			return deny("invalid value %s", v)
		}
		return check(o)
	}
}

// each checks every item of a field holding a list.
func each(check rule) rule {
	return func(v json.RawMessage) error {
		if isZero(v) {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(v, &items); err != nil {
			return deny("invalid value %s", v)
		}
		for _, it := range items {
			if err := check(it); err != nil {
				return err
			}
		}
		return nil
	}
}

// checkFields checks an object's fields by rules; a field without a rule
// must be empty. Docker reads field names regardless of case (Unicode
// folding, as strings.EqualFold), so the rules are found the same way.
func checkFields(where string, o map[string]json.RawMessage, rules map[string]rule) error {
	keys := slices.Sorted(maps.Keys(o))
	for i, k := range keys {
		for _, k2 := range keys[i+1:] {
			// Docker would take one of them, the proxy may check the other.
			if strings.EqualFold(k, k2) {
				return deny("%s%s is given twice", where, k)
			}
		}
	}
	for _, k := range keys {
		r, ok := rules[k]
		if !ok {
			for name, nr := range rules {
				if strings.EqualFold(name, k) {
					r, ok = nr, true
					break
				}
			}
		}
		if !ok {
			if !isZero(o[k]) {
				return deny("%s%s is not allowed", where, k)
			}
			continue
		}
		if err := r(o[k]); err != nil {
			return err
		}
	}
	return nil
}

// field is an object's field as Docker finds it: the name in any case.
func field(o map[string]json.RawMessage, name string) json.RawMessage {
	for k, v := range o {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

func isNull(v json.RawMessage) bool {
	s := string(bytes.TrimSpace(v))
	return s == "" || s == "null"
}

// isZero reports whether a JSON value is empty: null, false, 0, "", [] or
// {} (not an object of empty values: what an empty list in it means is
// up to the field).
func isZero(v json.RawMessage) bool {
	s := string(bytes.TrimSpace(v))
	switch s {
	case "", "null", "false", `""`:
		return true
	}
	if s[0] == '[' || s[0] == '{' {
		var a []json.RawMessage
		var o map[string]json.RawMessage
		return json.Unmarshal(v, &a) == nil && len(a) == 0 || json.Unmarshal(v, &o) == nil && len(o) == 0
	}
	var f float64
	return json.Unmarshal(v, &f) == nil && f == 0
}

var containerRules = map[string]rule{
	"Hostname": allow, "Domainname": allow, "User": allow,
	"AttachStdin": allow, "AttachStdout": allow, "AttachStderr": allow,
	"Tty": allow, "OpenStdin": allow, "StdinOnce": allow,
	"Env": allow, "Cmd": allow, "Entrypoint": allow, "Shell": allow, "ArgsEscaped": allow,
	"Image": allow, "Volumes": allow, "WorkingDir": allow, "Labels": allow,
	"ExposedPorts": allow, "Healthcheck": allow, "StopSignal": allow, "StopTimeout": allow,
	"NetworkDisabled":  allow,
	"HostConfig":       object(checkHostConfig),
	"NetworkingConfig": object(checkNetworkingConfig),
}

// checkContainerCreate checks the body of a container create.
func checkContainerCreate(o map[string]json.RawMessage) error {
	return checkFields("", o, containerRules)
}

var hostConfigRules = map[string]rule{
	"ContainerIDFile": allow,
	"LogConfig":       object(checkLogConfig),
	"NetworkMode":     checkNetworkMode,
	"PortBindings":    allow, "PublishAllPorts": allow,
	"RestartPolicy": allow, "AutoRemove": allow, "ConsoleSize": allow,
	"CapDrop": allow,
	"Dns":     allow, "DnsOptions": allow, "DnsSearch": allow, "ExtraHosts": allow,
	"GroupAdd": allow,
	"OomScoreAdj": func(v json.RawMessage) error {
		var n int
		if json.Unmarshal(v, &n) != nil || n < 0 {
			return deny("HostConfig.OomScoreAdj %s is not allowed", v)
		}
		return nil
	},
	"ReadonlyRootfs": allow, "ShmSize": allow, "Init": allow, "Tmpfs": allow,
	"CpuShares": allow, "Memory": allow, "NanoCpus": allow, "CpuPeriod": allow, "CpuQuota": allow,
	"CpusetCpus": allow, "CpusetMems": allow, "MemoryReservation": allow, "MemorySwap": allow,
	"MemorySwappiness": allow, "PidsLimit": allow, "Ulimits": allow, "BlkioWeight": allow,
	"Mounts": each(checkMount),
	"SecurityOpt": each(func(v json.RawMessage) error {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return deny("HostConfig.SecurityOpt %s is not allowed", v)
		}
		return checkSecurityOpt(s)
	}),
	"IpcMode":       oneOf("HostConfig.IpcMode", "private", "shareable", "none"),
	"CgroupnsMode":  oneOf("HostConfig.CgroupnsMode", "private"),
	"Isolation":     oneOf("HostConfig.Isolation", "default"),
	"Runtime":       oneOf("HostConfig.Runtime", "runc"),
	"MaskedPaths":   null("HostConfig.MaskedPaths"),
	"ReadonlyPaths": null("HostConfig.ReadonlyPaths"),
}

// checkHostConfig checks a container's HostConfig: no privileges, host
// namespaces, host paths or devices (all of those have no rule, so they
// must be empty).
func checkHostConfig(o map[string]json.RawMessage) error {
	return checkFields("HostConfig.", o, hostConfigRules)
}

func checkLogConfig(o map[string]json.RawMessage) error {
	// Drivers that write on the host only (others send logs over the network).
	return checkFields("HostConfig.LogConfig.", o, map[string]rule{
		"Type":   oneOf("the log driver", "json-file", "local", "journald", "none"),
		"Config": allow,
	})
}

func checkNetworkMode(v json.RawMessage) error {
	var s string
	if json.Unmarshal(v, &s) != nil {
		return deny("HostConfig.NetworkMode %s is not allowed", v)
	}
	if s == "host" || strings.Contains(s, ":") {
		return deny("containers may not use the network %q", s)
	}
	return nil
}

// checkSecurityOpt accepts only no-new-privileges: everything else
// (seccomp, apparmor, labels, systempaths) loosens the confinement.
func checkSecurityOpt(s string) error {
	switch s {
	case "no-new-privileges", "no-new-privileges:true", "no-new-privileges=true":
		return nil
	}
	return deny("HostConfig.SecurityOpt %q is not allowed", s)
}

// checkMount accepts the agent's volumes ("basalt-…", or anonymous ones)
// and tmpfs: never a host path, an image, or a volume of another driver.
func checkMount(v json.RawMessage) error {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(v, &o); err != nil {
		return deny("invalid mount %s", v)
	}
	if err := checkFields("HostConfig.Mounts.", o, map[string]rule{
		"Type":   oneOf("the mount type", "volume", "tmpfs"),
		"Source": allow, "Target": allow, "ReadOnly": allow, "Consistency": allow,
		"VolumeOptions": object(func(o map[string]json.RawMessage) error {
			return checkFields("HostConfig.Mounts.VolumeOptions.", o, map[string]rule{
				"NoCopy": allow, "Labels": allow, "Subpath": allow,
			})
		}),
		"TmpfsOptions": allow,
	}); err != nil {
		return err
	}
	var typ, src string
	json.Unmarshal(field(o, "Type"), &typ)
	json.Unmarshal(field(o, "Source"), &src)
	if typ == "" {
		return deny("a mount without a type is not allowed")
	}
	if typ == "volume" && src != "" && (!strings.HasPrefix(src, "basalt-") || strings.HasSuffix(src, BuildKitStateSuffix)) {
		return deny("the volume %q is not Basalt's", src)
	}
	return nil
}

// BuildKitStateSuffix ends the name of the BuildKit container's volume,
// which no other container may mount.
const BuildKitStateSuffix = "-buildkit-state"

var endpointRules = map[string]rule{
	"Aliases": allow, "DNSNames": allow, "IPAMConfig": allow, "NetworkID": allow, "EndpointID": allow,
	"Gateway": allow, "IPAddress": allow, "IPPrefixLen": allow, "IPv6Gateway": allow,
	"GlobalIPv6Address": allow, "GlobalIPv6PrefixLen": allow, "MacAddress": allow, "GwPriority": allow,
}

func checkNetworkingConfig(o map[string]json.RawMessage) error {
	return checkFields("NetworkingConfig.", o, map[string]rule{
		"EndpointsConfig": object(func(eps map[string]json.RawMessage) error {
			for name, ep := range eps {
				if name == "host" || strings.Contains(name, ":") {
					return deny("containers may not join the network %q", name)
				}
				if err := object(func(o map[string]json.RawMessage) error {
					return checkFields("NetworkingConfig.EndpointsConfig.", o, endpointRules)
				})(ep); err != nil {
					return err
				}
			}
			return nil
		}),
	})
}

// checkExec checks the body of an exec create: never privileged.
func checkExec(o map[string]json.RawMessage) error {
	return checkFields("exec ", o, map[string]rule{
		"User": allow, "Tty": allow, "AttachStdin": allow, "AttachStdout": allow, "AttachStderr": allow,
		"DetachKeys": allow, "Env": allow, "WorkingDir": allow, "Cmd": allow, "ConsoleSize": allow,
	})
}

// checkNetworkCreate allows bridge networks without driver options.
func checkNetworkCreate(o map[string]json.RawMessage) error {
	return checkFields("network ", o, map[string]rule{
		"Name": allow, "CheckDuplicate": allow, "Driver": oneOf("the network driver", "bridge"),
		"Internal": allow, "Attachable": allow, "EnableIPv4": allow, "EnableIPv6": allow,
		"Labels": allow, "IPAM": allow,
	})
}

func checkNetworkConnect(o map[string]json.RawMessage) error {
	return checkFields("", o, map[string]rule{
		"Container": allow,
		"EndpointConfig": object(func(o map[string]json.RawMessage) error {
			return checkFields("EndpointConfig.", o, endpointRules)
		}),
	})
}

// checkVolumeCreate allows local volumes without driver options (which
// would mount any host path or device).
func checkVolumeCreate(o map[string]json.RawMessage) error {
	return checkFields("volume ", o, map[string]rule{
		"Name": allow, "Labels": allow, "Driver": oneOf("the volume driver", "local"),
	})
}
