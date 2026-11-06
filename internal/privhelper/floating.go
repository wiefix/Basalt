package privhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
)

// MsgFloatingIPs sets the floating IPs on the host's interface
// (agentproto.FloatingIPs).
const MsgFloatingIPs = "floating_ips"

// floatingState is what the helper added, so that it removes only those:
// an address configured by the host's own network setup stays.
type floatingState struct {
	Interface string   `json:"interface"`
	Addresses []string `json:"addresses"`
}

const fileFloating = "floating-ips.json"

// runIP runs iproute2's ip (the helper is the only one with the right to
// change the host's addresses).
func runIP(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "ip", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("ip %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// SetFloatingIPs puts the addresses on the interface of the default route
// and takes away the ones it added before that are no longer wanted.
// Without its own network setup they do not survive a reboot: the agent
// gives them again when it starts.
func (h *Helper) SetFloatingIPs(ctx context.Context, f agentproto.FloatingIPs) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if runtime.GOOS != "linux" && h.ip == nil {
		if len(f.Addresses) == 0 {
			return nil
		}
		return errors.New("floating IPs need Linux")
	}
	run := h.ip
	if run == nil {
		run = runIP
	}
	h.floatingMu.Lock()
	defer h.floatingMu.Unlock()

	statePath := filepath.Join(h.cfg.StateDir, fileFloating)
	var st floatingState
	if b, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			h.log.Warn("floating IPs: unreadable state, starting over", "err", err)
			st = floatingState{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	iface, err := defaultInterface(ctx, run)
	if err != nil {
		return err
	}
	// Removed first: the ones on another interface (the default route
	// moved) and the ones no longer wanted.
	var keep []string
	for _, a := range st.Addresses {
		if st.Interface == iface && slices.Contains(f.Addresses, a) {
			keep = append(keep, a)
			continue
		}
		have, err := addressesOn(ctx, run, st.Interface)
		if err == nil && have[a] != 0 {
			if _, err := run(ctx, "addr", "del", prefixOf(a, have[a]), "dev", st.Interface); err != nil {
				h.log.Warn("floating IP not removed", "address", a, "interface", st.Interface, "err", err)
				continue
			}
			h.log.Info("floating IP removed", "address", a, "interface", st.Interface)
		}
	}
	have, err := addressesOn(ctx, run, iface)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range f.Addresses {
		if have[a] != 0 {
			continue // ours from before, or the host's own setup
		}
		args := []string{"addr", "add", prefixOf(a, 0), "dev", iface}
		if strings.Contains(a, ":") {
			// The provider routes it to this server alone: no neighbour
			// on the link has it, and DAD would only delay it.
			args = append(args, "nodad")
		}
		if _, err := run(ctx, args...); err != nil {
			errs = append(errs, err)
			continue
		}
		h.log.Info("floating IP added", "address", a, "interface", iface)
		keep = append(keep, a)
	}
	slices.Sort(keep)
	b, err := json.Marshal(floatingState{Interface: iface, Addresses: slices.Compact(keep)})
	if err != nil {
		return err
	}
	if err := writeFile(statePath, b, 0o600); err != nil {
		return err
	}
	return errors.Join(errs...)
}

// prefixOf is an address with its prefix length (bits 0: a single address).
func prefixOf(a string, bits int) string {
	if bits == 0 {
		bits = 32
		if strings.Contains(a, ":") {
			bits = 128
		}
	}
	return fmt.Sprintf("%s/%d", a, bits)
}

// defaultInterface is the interface of the IPv4 default route, else of
// the IPv6 one.
func defaultInterface(ctx context.Context, run func(context.Context, ...string) ([]byte, error)) (string, error) {
	for _, family := range []string{"-4", "-6"} {
		out, err := run(ctx, family, "-j", "route", "show", "default")
		if err != nil {
			return "", err
		}
		var routes []struct {
			Dev string `json:"dev"`
		}
		if err := json.Unmarshal(out, &routes); err != nil {
			return "", fmt.Errorf("ip route: %w", err)
		}
		for _, r := range routes {
			if validInterface(r.Dev) {
				return r.Dev, nil
			}
		}
	}
	return "", errors.New("the host has no default route to put floating IPs on")
}

func validInterface(name string) bool {
	if name == "" || len(name) > 15 || strings.HasPrefix(name, "-") {
		return false
	}
	for _, c := range name {
		if c <= ' ' || c > '~' || c == '/' {
			return false
		}
	}
	return true
}

// addressesOn maps the addresses of an interface to their prefix lengths.
func addressesOn(ctx context.Context, run func(context.Context, ...string) ([]byte, error), iface string) (map[string]int, error) {
	if !validInterface(iface) {
		return map[string]int{}, nil
	}
	out, err := run(ctx, "-j", "addr", "show", "dev", iface)
	if err != nil {
		return nil, err
	}
	var links []struct {
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &links); err != nil {
		return nil, fmt.Errorf("ip addr: %w", err)
	}
	have := map[string]int{}
	for _, l := range links {
		for _, a := range l.AddrInfo {
			if p, err := netip.ParseAddr(a.Local); err == nil && a.PrefixLen > 0 {
				have[p.Unmap().String()] = a.PrefixLen
			}
		}
	}
	return have, nil
}
