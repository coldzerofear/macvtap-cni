// Copyright 2019 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cni

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/kubevirt/macvtap-cni/pkg/util"
	"github.com/vishvananda/netlink"
	"log"
	"net"
	"os"
	"runtime"
	"strings"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/utils/sysctl"
)

// A NetConf structure represents a Multus network attachment definition configuration
type NetConf struct {
	types.NetConf
	DeviceID      string `json:"deviceID"`
	MTU           int    `json:"mtu,omitempty"`
	IsPromiscuous bool   `json:"promiscMode,omitempty"`
	Mac           string `json:"mac,omitempty"`

	IsVmPod       bool `json:"isVmPod,omitempty"`
	RuntimeConfig struct {
		Mac string `json:"mac,omitempty"`
	} `json:"runtimeConfig,omitempty"`
}

// EnvArgs structure represents inputs sent from each VMI via environment variables
type EnvArgs struct {
	types.CommonArgs
	MAC          types.UnmarshallableString `json:"mac,omitempty"`
	K8S_POD_NAME types.UnmarshallableString `json:"k8s_pod_name,omitempty"`
}

var logger *log.Logger

func init() {
	logFile, err := os.OpenFile("/opt/cni/bin/macvtap.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Panic("打开日志文件异常")
	}
	logger = log.New(logFile, "[macvtap]", log.Ldate|log.Ltime|log.Lshortfile)
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func loadConf(bytes []byte, envArgs string) (*NetConf, string, error) {
	n := &NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, "", fmt.Errorf("failed to load netconf: %v", err)
	}
	if envArgs != "" {
		env, err := getEnvArgs(envArgs)
		if err != nil {
			return nil, "", err
		}
		if env.MAC != "" {
			n.Mac = string(env.MAC)
		}
		if env.K8S_POD_NAME != "" {
			n.IsVmPod = strings.HasPrefix(string(env.K8S_POD_NAME), "virt-launcher-")
		}
	}
	if n.RuntimeConfig.Mac != "" {
		n.Mac = n.RuntimeConfig.Mac
	}
	return n, n.CNIVersion, nil
}

func getEnvArgs(envArgsString string) (EnvArgs, error) {
	e := EnvArgs{}
	err := types.LoadArgs(envArgsString, &e)
	if err != nil {
		return e, err
	}
	return e, nil
}

// CmdAdd - CNI interface
func CmdAdd(args *skel.CmdArgs) error {
	logger.Println("CmdAdd")
	logger.Println(
		"ContainerID: ", args.ContainerID,
		"Netns: ", args.Netns,
		"IfName: ", args.IfName,
		"Args: ", args.Args,
		"Path: ", args.Path,
		"StdinData: ", string(args.StdinData))

	var err error
	netConf, cniVersion, err := loadConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	marshal, _ := json.Marshal(netConf)
	logger.Println("NetConf: ", string(marshal))

	var mac *net.HardwareAddr = nil
	if netConf.Mac != "" {
		aMac, err := net.ParseMAC(netConf.Mac)
		mac = &aMac
		if err != nil {
			return err
		}
	}

	isLayer3 := netConf.IPAM.Type != ""

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}

	// Delete link if err to avoid link leak in this ns
	defer func() {
		netns.Close()
		if err != nil {
			util.LinkDelete(netConf.DeviceID)
		}
	}()
	var macvtapInterface *current.Interface = nil
	if util.HasInterface(netConf.DeviceID) {
		// 网卡设备在host网络命名空间，将其移动到容器网络命名空间并配置
		macvtapInterface, err = util.ConfigureInterface(netConf.DeviceID, args.IfName, mac, netConf.MTU, netConf.IsPromiscuous, netns)
		if err != nil {
			return err
		}
	} else if util.HasInterfaceInNs(args.IfName, netns) {
		// 网卡设备在容器网络命名空间下
		if err = netns.Do(func(_ ns.NetNS) error {
			l, err := netlink.LinkByName(args.IfName)
			if err != nil {
				return err
			}
			macvtapInterface = &current.Interface{
				Name:    args.IfName,
				Mac:     l.Attrs().HardwareAddr.String(),
				Sandbox: netns.Path(),
			}
			return nil
		}); err != nil {
			return err
		}
	} else {
		// 没找到网卡设备则报错
		return fmt.Errorf("failed to lookup device %q: %s", netConf.DeviceID, "Link not found")
	}

	// Assume L2 interface only
	result := &current.Result{
		CNIVersion: cniVersion,
		Interfaces: []*current.Interface{macvtapInterface},
	}

	if isLayer3 {
		logger.Println("Need ", netConf.IPAM.Type, " IPAM to allocate address")

		// run the IPAM plugin and get back the config to apply
		r, err := ipam.ExecAdd(netConf.IPAM.Type, args.StdinData)
		if err != nil {
			logger.Println("Exec IPAM plugin error: ", err.Error())
			return err
		}

		// Invoke ipam del if err to avoid ip leak
		defer func() {
			if err != nil {
				ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
			}
		}()

		// Convert whatever the IPAM result was into the current Result type
		ipamResult, err := current.NewResultFromResult(r)
		if err != nil {
			logger.Println("Convert IPAM result error: ", err.Error())
			return err
		}

		irs, _ := json.Marshal(ipamResult)
		logger.Println("ipamResult: ", string(irs))

		// 没有分配到IP只抛出错误不回收连接
		if len(ipamResult.IPs) == 0 {
			return errors.New("IPAM plugin returned missing IP config")
		}

		ipamMacStr := ""
		for _, iface := range ipamResult.Interfaces {
			if iface != nil && iface.Mac != "" {
				ipamMacStr = iface.Mac
				macAddr, err := net.ParseMAC(iface.Mac)
				if err != nil {
					logger.Println("failed to parse mac address: ", err.Error())
					//return fmt.Errorf("failed to parse mac address: %v", err)
					continue
				}
				mac = &macAddr
				break
			}
		}

		// reset mac
		if ipamMacStr != "" && !strings.EqualFold(ipamMacStr, netConf.Mac) {
			if err = util.SetInterfaceMacAddress(args.IfName, mac, netns); err != nil {
				logger.Println("failed to set mac address: ", err.Error())
				return err
			}
			macvtapInterface.Mac = ipamMacStr
		}

		result.IPs = ipamResult.IPs
		result.Routes = ipamResult.Routes
		result.DNS = ipamResult.DNS

		if err1 := netns.Do(func(_ ns.NetNS) error {
			_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/arp_notify", args.IfName), "1")
			if netConf.IsVmPod {
				// TODO 在vmpod下网卡保持原配
				return ipam.ConfigureIface(args.IfName, ipamResult)
			} else {
				// TODO 在普通pod下配置其网卡属性
				return ipam.ConfigureIface(args.IfName, result)
			}
			//for _, ipc := range result.IPs {
			//	if ipc.Version == "4" {
			//		arperr := arping.GratuitousArpOverIfaceByName(ipc.Address.IP, args.IfName)
			//		if arperr != nil {
			//			logger.Println("arping GratuitousArpOverIfaceByName invoke error: ", arperr.Error())
			//		}
			//	}
			//}
		}); err1 != nil {
			logger.Println("Configure ipam error: ", err1.Error())
			// return err
		}
	}
	rs, _ := json.Marshal(result)
	logger.Println("CmdAdd result: ", string(rs))
	return types.PrintResult(result, cniVersion)
}

// CmdDel - CNI plugin Interface
func CmdDel(args *skel.CmdArgs) error {
	netConf, _, err := loadConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}
	isLayer3 := netConf.IPAM.Type != ""

	if isLayer3 {
		err = ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	if args.Netns == "" {
		return nil
	}

	// There is a netns so try to clean up. Delete can be called multiple times
	// so don't return an error if the device is already removed.
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {

		if err := ip.DelLinkByName(args.IfName); err != nil {
			if err != ip.ErrLinkNotFound {
				return err
			}
		}
		return nil
	})

	return err
}

// CmdCheck - CNI plugin Interface
func CmdCheck(args *skel.CmdArgs) error {
	return nil
}
