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
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/utils/sysctl"
	"github.com/kubevirt/macvtap-cni/pkg/util"
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
		log.Panic("Initialization log file error", err)
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

	var (
		netConf    *NetConf
		cniVersion string
		mac        *net.HardwareAddr
		err        error
	)
	netConf, cniVersion, err = loadConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	netConfBytes, _ := json.Marshal(netConf)
	logger.Println("Add NetConf: ", string(netConfBytes))

	if netConf.Mac != "" {
		aMac, err := net.ParseMAC(netConf.Mac)
		mac = &aMac
		if err != nil {
			return err
		}
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	// Close netns context
	defer netns.Close()

	var (
		macvtapInterface *current.Interface
		result           = &current.Result{CNIVersion: cniVersion}
		isLayer3         = netConf.IPAM.Type != ""
	)

	if isLayer3 {
		logger.Println("Need ", netConf.IPAM.Type, " IPAM to allocate address")

		ipamResult, err := util.ExecIPAMAdd(netConf.IPAM.Type, args.StdinData)
		if err != nil {
			logger.Println(err)
			return err
		}

		ipamBytes, _ := json.Marshal(ipamResult)
		logger.Println("ipamResult: ", string(ipamBytes))

		// Prioritize using the MAC address distributed by IPAM
		for _, iface := range ipamResult.Interfaces {
			if iface != nil && len(iface.Mac) > 0 {
				macAddr, err := net.ParseMAC(iface.Mac)
				if err != nil {
					logger.Println("failed to parse mac address: ", err.Error())
					continue
				}
				mac = &macAddr
				break
			}
		}

		result.IPs = ipamResult.IPs
		result.Routes = ipamResult.Routes
		result.DNS = ipamResult.DNS

	}

	// Delete link if err to avoid link leak in this ns
	defer func() {
		if err != nil {
			// Invoke ipam del if err to avoid ip leak
			if isLayer3 {
				ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
			}
			// remember to destroy devices that have been moved to the container netns
			netns.Do(func(_ ns.NetNS) error {
				return util.LinkDelete(args.IfName)
			})
			util.LinkDelete(netConf.DeviceID)
		}
	}()

	macvtapInterface, err = util.ConfigureInterface(netConf.DeviceID, args.IfName, mac, netConf.MTU, netConf.IsPromiscuous, netns)
	if err != nil {
		logger.Println(err)
		return err
	}

	result.Interfaces = []*current.Interface{macvtapInterface}

	if isLayer3 {
		setIPAMResultErr := netns.Do(func(_ ns.NetNS) error {
			_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/arp_notify", args.IfName), "1")
			if !netConf.IsVmPod {
				// TODO 普通pod下配置网卡
				return ipam.ConfigureIface(args.IfName, result)
			}
			// TODO vm pod下保持原网卡属性
			return nil
		})
		if setIPAMResultErr != nil {
			logger.Println("Configure IPAM result error: ", setIPAMResultErr.Error())
		}
	}

	rs, _ := json.Marshal(result)
	logger.Println("CmdAdd result: ", string(rs))

	return types.PrintResult(result, cniVersion)
}

// CmdDel - CNI plugin Interface
func CmdDel(args *skel.CmdArgs) error {
	logger.Println("CmdDel")
	logger.Println(
		"ContainerID: ", args.ContainerID,
		"Netns: ", args.Netns,
		"IfName: ", args.IfName,
		"Args: ", args.Args,
		"Path: ", args.Path,
		"StdinData: ", string(args.StdinData))

	netConf, _, err := loadConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	netConfBytes, _ := json.Marshal(netConf)
	logger.Println("Del NetConf: ", string(netConfBytes))

	isLayer3 := netConf.IPAM.Type != ""

	if isLayer3 {
		err = ipam.ExecDel(netConf.IPAM.Type, args.StdinData)
		if err != nil {
			logger.Println("Exec IPAM delete error: ", err.Error())
			return err
		}
	}

	if args.Netns == "" {
		return nil
	}

	// There is a netns so try to clean up. Delete can be called multiple times
	// so don't return an error if the device is already removed.
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		err := ip.DelLinkByName(args.IfName)
		if err != nil && err != ip.ErrLinkNotFound {
			return err
		}
		return nil
	})
	if err != nil {
		logger.Println("Delete netns ", args.Netns, " macvtap link ", args.IfName, " error: ", err.Error())
	}
	return err
}

// CmdCheck - CNI plugin Interface
func CmdCheck(args *skel.CmdArgs) error {
	return nil
}
