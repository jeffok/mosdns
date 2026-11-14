/* Copyright (C) 2026 jeffok. SPDX-License-Identifier: GPL-3.0-or-later */
package ros_bgp

import (
	"bufio"
	"fmt"
	"github.com/IrineSistiana/mosdns/v5/coremain"
	"go4.org/netipx"
	"net/netip"
	"os"
	"strings"
)

const PluginType = "ros_bgp"

type Args struct {
	Mode        string `yaml:"mode"`
	Endpoint    string `yaml:"endpoint"`
	SyncBudget  string `yaml:"sync_budget"`
	CNIPFile    string `yaml:"cn_ip_file"`
	GuardWriter bool   `yaml:"guard_writer"`
}

func init() { coremain.RegNewPluginFunc(PluginType, Init, func() any { return new(Args) }) }
func Init(_ *coremain.BP, args any) (any, error) {
	a := args.(*Args)
	switch a.Mode {
	case "tag":
		return &bgpTagPlugin{}, nil
	case "direct_tag":
		return &bgpDirectTagPlugin{}, nil
	case "confirm":
		return newBGPConfirm(a)
	default:
		return nil, fmt.Errorf("ros_bgp: invalid mode %q", a.Mode)
	}
}
func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func loadCN(path string) (*netipx.IPSet, error) {
	if path == "" {
		return nil, fmt.Errorf("ros_bgp: cn_ip_file empty")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b netipx.IPSetBuilder
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.Fields(line)[0]
		p, err := netip.ParsePrefix(line)
		if err != nil {
			addr, e := netip.ParseAddr(line)
			if e != nil {
				continue
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		b.AddPrefix(p)
		n++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("ros_bgp: empty CN prefix set")
	}
	return b.IPSet()
}
