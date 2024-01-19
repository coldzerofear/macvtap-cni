package main

import (
	"flag"
	"os"

	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	macvtap "github.com/kubevirt/macvtap-cni/pkg/deviceplugin"
	"github.com/kubevirt/macvtap-cni/pkg/util"
)

func main() {
	AddFlags(flag.CommandLine)
	flag.Parse()
	// Device plugin operates with several goroutines that might be
	// relocated among different OS threads with different namespaces.
	// We capture the main namespace here and make sure that we do any
	// network operation on that namespace.
	// See
	// https://github.com/containernetworking/plugins/blob/master/pkg/ns/README.md
	mainNsPath := util.GetMainThreadNetNsPath()

	listerType := macvtap.ListerTypeConfigEnv
	_, configDefined := os.LookupEnv(macvtap.EnvName)
	if !configDefined {
		glog.Warningf("not found environment variable [%s], try use ConfigPath mode", macvtap.EnvName)
		//glog.Exitf("%s environment variable must be set", macvtap.ConfigEnvironmentVariable)
		listerType = macvtap.ListerTypeConfigPath
	}
	glog.Infoln("current use lister type: ", listerType)
	manager := dpm.NewManager(macvtap.NewMacvtapLister(mainNsPath, listerType))
	manager.Run()
}

func AddFlags(fs *flag.FlagSet) {
	fs.StringVar(&macvtap.EnvName, "env-name", macvtap.ConfigEnvironmentVariable, "Custom config environment name")
	fs.StringVar(&macvtap.ConfigMapFilePath, "config-path", macvtap.ConfigMapDefaultPath, "Custom config file path")
	fs.BoolVar(&macvtap.SortDeviceIds, "sort-devices", true, "Enable preferred allocation sort device ids")
}
