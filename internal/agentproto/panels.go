package agentproto

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// A panel can be replaced by one of its standby panels (`basalt server
// promote`). Each promotion raises the panel's epoch. Agents learn the
// addresses of the panel and its standbys and the epoch (PanelInfo), try
// the other addresses when the one they use fails, and tell the panel the
// epoch they know when they connect: a panel with a lower epoch was
// replaced and refuses them (409), so a former panel that comes back
// never takes its agents back.

// PathPanelEpoch answers the panel's epoch on its agent port (GET, JSON
// {"epoch": n}, no client certificate): a former panel asks its standbys
// whether one took over.
const PathPanelEpoch = "/agent/v1/epoch"

// TypePanelInfo tells an agent the panel's epoch and addresses (PanelInfo).
const TypePanelInfo = "panel_info"

// HeaderPanelEpoch carries the highest epoch an agent knows when it
// connects.
const HeaderPanelEpoch = "Basalt-Panel-Epoch"

// MaxPanels bounds the addresses an agent keeps.
const MaxPanels = 8

type PanelInfo struct {
	Epoch int64 `json:"epoch"`
	// Panels are the agent ports of the panel (first) and its standbys,
	// https://host:port.
	Panels []string `json:"panels"`
}

// ValidPanelURL checks an address of a panel's agent port.
func ValidPanelURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil:
		return errors.New("a panel address is https://host[:port]")
	case strings.TrimSuffix(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "":
		return errors.New("a panel address has no path")
	case len(s) > 300:
		return errors.New("a panel address is too long")
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil && !strings.Contains(err.Error(), "missing port") {
		return errors.New("a panel address has an invalid port")
	}
	return nil
}
