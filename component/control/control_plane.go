/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) since 2022, mzz2017 (mzz@tuta.io). All rights reserved.
 */

package control

import (
	"context"
	"errors"
	"fmt"
	"foo/common"
	"foo/common/consts"
	"foo/component/outbound"
	"foo/component/outbound/dialer"
	"foo/component/routing"
	"foo/pkg/pool"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type ControlPlane struct {
	log *logrus.Logger

	// TODO: add mutex?
	outbounds       []*outbound.DialerGroup
	outboundName2Id map[string]uint8
	bpf             *bpfObjects

	SimulatedLpmTries  [][]netip.Prefix
	SimulatedDomainSet []DomainSet
	Final              string

	// mutex protects the dnsCache.
	mutex    sync.Mutex
	dnsCache map[string]*dnsCache
	epoch    uint32

	deferFuncs []func() error
}

func NewControlPlane(log *logrus.Logger, routingA string) (*ControlPlane, error) {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("rlimit.RemoveMemlock:%v", err)
	}
	pinPath := filepath.Join(consts.BpfPinRoot, consts.AppName)
	os.MkdirAll(pinPath, 0755)
	// Load pre-compiled programs and maps into the kernel.
	var bpf bpfObjects
retry_load:
	if err := loadBpfObjects(&bpf, &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			PinPath: pinPath,
		},
	}); err != nil {
		if errors.Is(err, ebpf.ErrMapIncompatible) {
			prefix := "use pinned map "
			iPrefix := strings.Index(err.Error(), prefix)
			if iPrefix == -1 {
				return nil, fmt.Errorf("loading objects: bad format: %w", err)
			}
			mapName := strings.SplitN(err.Error()[iPrefix+len(prefix):], ":", 2)[0]
			_ = os.Remove(filepath.Join(pinPath, mapName))
			log.Warnf("New map format was incompatible with existing map %v, and the old one was removed.", mapName)
			goto retry_load
		}
		return nil, fmt.Errorf("loading objects: %w", err)
	}

	// Flush dst_map.
	//_ = os.Remove(filepath.Join(pinPath, "dst_map"))
	//if err := bpf.ParamMap.Update(consts.IpsLenKey, uint32(1), ebpf.UpdateAny); err != nil {
	//	return nil, err
	//}
	//if err := bpf.ParamMap.Update(consts.BigEndianTproxyPortKey, uint32(swap16(tproxyPort)), ebpf.UpdateAny); err != nil {
	//	return nil, err
	//}
	if err := bpf.ParamMap.Update(consts.DisableL4TxChecksumKey, consts.DisableL4ChecksumPolicy_SetZero, ebpf.UpdateAny); err != nil {
		return nil, err
	}
	if err := bpf.ParamMap.Update(consts.DisableL4RxChecksumKey, consts.DisableL4ChecksumPolicy_SetZero, ebpf.UpdateAny); err != nil {
		return nil, err
	}
	var epoch uint32
	bpf.ParamMap.Lookup(consts.EpochKey, &epoch)
	epoch++
	if err := bpf.ParamMap.Update(consts.EpochKey, epoch, ebpf.UpdateAny); err != nil {
		return nil, err
	}
	//if err := bpf.ParamMap.Update(consts.InterfaceIpParamOff, binary.LittleEndian.Uint32([]byte{172, 17, 0, 1}), ebpf.UpdateAny); err != nil { // 172.17.0.1
	//	return nil, err
	//}
	//if err := bpf.ParamMap.Update(InterfaceIpParamOff+1, binary.LittleEndian.Uint32([]byte{10, 249, 40, 166}), ebpf.UpdateAny); err != nil { // 10.249.40.166
	//	log.Println(err)
	//	return
	//}
	//if err := bpf.ParamMap.Update(InterfaceIpParamOff+2, binary.LittleEndian.Uint32([]byte{10, 250, 52, 180}), ebpf.UpdateAny); err != nil { // 10.250.52.180
	//	log.Println(err)
	//	return
	//}

	/**/

	rules, final, err := routing.Parse(routingA)
	if err != nil {
		return nil, fmt.Errorf("routingA error: \n %w", err)
	}
	if rules, err = routing.ApplyRulesOptimizers(rules,
		&routing.RefineFunctionParamKeyOptimizer{},
		&routing.DatReaderOptimizer{Logger: log},
		&routing.MergeAndSortRulesOptimizer{},
		&routing.DeduplicateParamsOptimizer{},
	); err != nil {
		return nil, fmt.Errorf("ApplyRulesOptimizers error: \n %w", err)
	}
	if log.IsLevelEnabled(logrus.TraceLevel) {
		var debugBuilder strings.Builder
		for _, rule := range rules {
			debugBuilder.WriteString(rule.String(true))
		}
		log.Tracef("RoutingA:\n%vfinal: %v\n", debugBuilder.String(), final)
	}
	// TODO:
	d, err := dialer.NewFromLink("socks5://localhost:1080")
	if err != nil {
		return nil, err
	}
	outbounds := []*outbound.DialerGroup{
		outbound.NewDialerGroup(log, consts.OutboundDirect.String(),
			[]*dialer.Dialer{dialer.FullconeDirectDialer},
			outbound.DialerSelectionPolicy{
				Policy:     consts.DialerSelectionPolicy_Fixed,
				FixedIndex: 0,
			}),
		outbound.NewDialerGroup(log, "proxy",
			[]*dialer.Dialer{d},
			outbound.DialerSelectionPolicy{
				Policy: consts.DialerSelectionPolicy_MinAverage10Latencies,
			}),
	}
	// Generate outboundName2Id from outbounds.
	if len(outbounds) > 0xff {
		return nil, fmt.Errorf("too many outbounds")
	}
	outboundName2Id := make(map[string]uint8)
	for i, o := range outbounds {
		outboundName2Id[o.Name] = uint8(i)
	}
	builder := NewRoutingMatcherBuilder(outboundName2Id, &bpf)
	if err := routing.ApplyMatcherBuilder(builder, rules, final); err != nil {
		return nil, fmt.Errorf("ApplyMatcherBuilder: %w", err)
	}
	if err := builder.Build(); err != nil {
		return nil, fmt.Errorf("RoutingMatcherBuilder.Build: %w", err)
	}
	/**/

	return &ControlPlane{
		log:                log,
		outbounds:          outbounds,
		outboundName2Id:    outboundName2Id,
		bpf:                &bpf,
		SimulatedLpmTries:  builder.SimulatedLpmTries,
		SimulatedDomainSet: builder.SimulatedDomainSet,
		Final:              final,
		mutex:              sync.Mutex{},
		dnsCache:           make(map[string]*dnsCache),
		epoch:              epoch,
		deferFuncs:         []func() error{bpf.Close},
	}, nil
}

func (c *ControlPlane) BindLink(ifname string) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return err
	}
	// Insert an elem into IfindexIpsMap.
	// TODO: We should monitor IP change of the link.
	ipnets, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	// TODO: If we monitor IP change of the link, we should remove code below.
	if len(ipnets) == 0 {
		return fmt.Errorf("interface %v has no ip", ifname)
	}
	var linkIp bpfIfIp
	for _, ipnet := range ipnets {
		ip, ok := netip.AddrFromSlice(ipnet.IP)
		if !ok {
			continue
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		if (ip.Is6() && linkIp.HasIp6) ||
			(ip.Is4() && linkIp.HasIp4) {
			continue
		}
		ip6format := ip.As16()
		if ip.Is4() {
			linkIp.HasIp4 = true
			linkIp.Ip4 = common.Ipv6ByteSliceToUint32Array(ip6format[:])
		} else {
			linkIp.HasIp6 = true
			linkIp.Ip6 = common.Ipv6ByteSliceToUint32Array(ip6format[:])
		}
		if linkIp.HasIp4 && linkIp.HasIp6 {
			break
		}
	}
	if err := c.bpf.IfindexIpMap.Update(uint32(link.Attrs().Index), linkIp, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("update IfindexIpsMap: %w", err)
	}

	// Insert qdisc and filters.
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: link.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	if err := netlink.QdiscAdd(qdisc); err != nil {
		if os.IsExist(err) {
			_ = netlink.QdiscDel(qdisc)
			err = netlink.QdiscAdd(qdisc)
		}

		if err != nil {
			return fmt.Errorf("cannot add clsact qdisc: %w", err)
		}
	}
	c.deferFuncs = append(c.deferFuncs, func() error {
		return netlink.QdiscDel(qdisc)
	})

	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    netlink.MakeHandle(0, 1),
			Protocol:  unix.ETH_P_ALL,
			Priority:  0,
		},
		Fd:           c.bpf.bpfPrograms.TproxyIngress.FD(),
		Name:         consts.AppName + "_ingress",
		DirectAction: true,
	}
	if err := netlink.FilterAdd(filter); err != nil {
		return fmt.Errorf("cannot attach ebpf object to filter ingress: %w", err)
	}
	c.deferFuncs = append(c.deferFuncs, func() error {
		return netlink.FilterDel(filter)
	})
	filterEgress := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_EGRESS,
			Handle:    netlink.MakeHandle(0, 1),
			Protocol:  unix.ETH_P_ALL,
			Priority:  0,
		},
		Fd:           c.bpf.bpfPrograms.TproxyEgress.FD(),
		Name:         consts.AppName + "_egress",
		DirectAction: true,
	}
	if err := netlink.FilterAdd(filterEgress); err != nil {
		return fmt.Errorf("cannot attach ebpf object to filter ingress: %w", err)
	}
	c.deferFuncs = append(c.deferFuncs, func() error {
		return netlink.FilterDel(filter)
	})
	return nil
}

func (c *ControlPlane) ListenAndServe(port uint16) (err error) {
	// Listen.
	listener, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(int(port)))
	if err != nil {
		return fmt.Errorf("listenTCP: %w", err)
	}
	defer listener.Close()
	lConn, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.IP{0, 0, 0, 0},
		Port: int(port),
	})
	if err != nil {
		return fmt.Errorf("listenUDP: %w", err)
	}
	defer lConn.Close()

	// Serve.
	if err := c.bpf.ParamMap.Update(consts.BigEndianTproxyPortKey, uint32(swap16(port)), ebpf.UpdateAny); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.deferFuncs = append(c.deferFuncs, func() error {
		cancel()
		return nil
	})
	go func() {
		defer cancel()
		for {
			lconn, err := listener.Accept()
			if err != nil {
				if !strings.Contains(err.Error(), "use of closed network connection") {
					c.log.Errorf("Error when accept: %v", err)
				}
				break
			}
			go func() {
				if err := c.handleConn(lconn); err != nil {
					c.log.Warnln("handleConn:", err)
				}
			}()
		}
	}()
	go func() {
		defer cancel()
		for {
			var buf [65536]byte
			n, lAddrPort, err := lConn.ReadFromUDPAddrPort(buf[:])
			if err != nil {
				if !strings.Contains(err.Error(), "use of closed network connection") {
					c.log.Errorf("ReadFromUDPAddrPort: %v, %v", lAddrPort.String(), err)
				}
				break
			}
			addrHdr, dataOffset, err := ParseAddrHdr(buf[:n])
			if err != nil {
				c.log.Warnf("No AddrPort presented")
				continue
			}
			newBuf := pool.Get(n - dataOffset)
			copy(newBuf, buf[dataOffset:n])
			go func(data []byte, lConn *net.UDPConn, lAddrPort netip.AddrPort, addrHdr *AddrHdr) {
				if e := c.handlePkt(newBuf, lConn, lAddrPort, addrHdr); e != nil {
					c.log.Warnln("handlePkt:", e)
				}
				pool.Put(newBuf)
			}(newBuf, lConn, lAddrPort, addrHdr)
		}
	}()
	<-ctx.Done()
	return nil
}

func (c *ControlPlane) Close() (err error) {
	// Invoke defer funcs in reverse order.
	for i := len(c.deferFuncs) - 1; i >= 0; i-- {
		if e := c.deferFuncs[i](); e != nil {
			// Combine errors.
			if err != nil {
				err = fmt.Errorf("%w; %v", err, e)
			} else {
				err = e
			}
		}
	}
	return err
}