package deviceplugin

import (
	"sync"

	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
)

var (
	ConfigMapFilePath string
	EnvName           string
	SortDeviceIds     bool
)

const (
	resourceNamespace         = "macvtap.network.kubevirt.io"
	ConfigEnvironmentVariable = "DP_MACVTAP_CONF"
	ConfigMapDefaultPath      = "/macvtap-deviceplugin-config/" + ConfigEnvironmentVariable

	ListerTypeConfigEnv  = "configEnv"
	ListerTypeConfigPath = "configPath"
)

type Config struct {
	Name        string `json:"name"`
	LowerDevice string `json:"lowerDevice"`
	Mode        string `json:"mode"`
	Capacity    int    `json:"capacity"`
}

type macvtapConfig struct {
	sync.RWMutex
	Config
	update chan struct{}
}

type macvtapLister struct {
	sync.RWMutex
	Config map[string]*macvtapConfig
	// NetNsPath is the path to the network namespace the lister operates in.
	NetNsPath string
	Type      string
}

func NewMacvtapLister(netNsPath, listerType string) *macvtapLister {
	return &macvtapLister{
		NetNsPath: netNsPath,
		Type:      listerType,
		Config:    make(map[string]*macvtapConfig),
	}
}

func (ml macvtapLister) GetResourceNamespace() string {
	return resourceNamespace
}

func (ml *macvtapLister) Discover(pluginListCh chan dpm.PluginNameList) {
	switch ml.Type {
	case ListerTypeConfigEnv:
		ml.ConfigEnvDiscover(pluginListCh)
	case ListerTypeConfigPath:
		ml.ConfigPathDiscover(pluginListCh)
	}
}

func (ml *macvtapLister) NewPlugin(name string) dpm.PluginInterface {
	ml.RLock()
	defer ml.RUnlock()
	cfg, ok := ml.Config[name]
	if !ok {
		cfg = &macvtapConfig{
			Config: Config{
				Name:        name,
				LowerDevice: name,
				Mode:        DefaultMode,
				Capacity:    DefaultCapacity,
			},
			update: make(chan struct{}),
		}
	}
	glog.V(3).Infof("Creating device plugin with config %+v", cfg)
	return NewMacvtapDevicePlugin(cfg, ml.NetNsPath, SortDeviceIds)
}
