package deviceplugin

import (
	"fmt"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	"sort"

	"github.com/kubevirt/macvtap-cni/pkg/util"
)

const (
	tapPath = "/dev/tap"
	// Interfaces will be named as <Name><suffix>[0-<Capacity>]
	suffix = "Mvp"
	// DefaultCapacity is the default when no capacity is provided
	DefaultCapacity = 100
	// DefaultMode is the default when no mode is provided
	DefaultMode = "bridge"
)

type macvtapDevicePlugin struct {
	*macvtapConfig
	preferredAllocation bool
	// NetNsPath is the path to the network namespace the plugin operates in.
	NetNsPath   string
	stopWatcher chan struct{}
}

func NewMacvtapDevicePlugin(config *macvtapConfig, netNsPath string, sort bool) *macvtapDevicePlugin {
	return &macvtapDevicePlugin{
		macvtapConfig:       config,
		preferredAllocation: sort,
		NetNsPath:           netNsPath,
		stopWatcher:         make(chan struct{}),
	}
}

func (mdp *macvtapDevicePlugin) generateMacvtapDevices() []*pluginapi.Device {
	var macvtapDevs []*pluginapi.Device

	var capacity = mdp.Capacity
	if capacity <= 0 {
		capacity = DefaultCapacity
	}

	for i := 0; i < capacity; i++ {
		name := fmt.Sprint(mdp.Name, suffix, i)
		macvtapDevs = append(macvtapDevs, &pluginapi.Device{
			ID:     name,
			Health: pluginapi.Healthy,
		})
	}

	return macvtapDevs
}

func (mdp *macvtapDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	// Initialize two arrays, one for devices offered when lower device exists,
	// and no devices if lower device does not exist.

	onLowerDeviceEvent := func() {
		mdp.RLock()
		defer mdp.RUnlock()

		doesLowerDeviceExist := false
		err := ns.WithNetNSPath(mdp.NetNsPath, func(_ ns.NetNS) error {
			var err error
			doesLowerDeviceExist, err = util.LinkExists(mdp.LowerDevice)
			return err
		})
		if err != nil {
			glog.Warningf("Error while checking on lower device %s: %v", mdp.LowerDevice, err)
			return
		}
		var allocatableDevs []*pluginapi.Device
		if doesLowerDeviceExist {
			glog.V(3).Infof("LowerDevice %s exists, sending ListAndWatch response with available devices", mdp.LowerDevice)
			allocatableDevs = mdp.generateMacvtapDevices()
		} else {
			glog.V(3).Info("LowerDevice %s does not exist, sending ListAndWatch response with no devices", mdp.LowerDevice)
			allocatableDevs = make([]*pluginapi.Device, 0)
		}
		_ = s.Send(&pluginapi.ListAndWatchResponse{Devices: allocatableDevs})
	}

loop:
	stopCh := make(chan struct{})
	// Listen for events of lower device interface. On any, check if lower
	// device exists. If it does, offer up to capacity macvtap devices. Do
	// not offer any otherwise.

	go util.OnLinkEvent(
		mdp.LowerDevice,
		mdp.NetNsPath,
		onLowerDeviceEvent,
		stopCh,
		func(err error) {
			glog.Error(err)
		})

	for {
		select {
		case <-mdp.update:
			close(stopCh)
			onLowerDeviceEvent()
			goto loop
		case <-mdp.stopWatcher:
			close(stopCh)
			glog.Warningf("Stop device plugin name: %s, lowerDevice: %s", mdp.Name, mdp.LowerDevice)
			return nil
		}
	}
}

func (mdp *macvtapDevicePlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	glog.Info("assign macvtap network devices: ", &r.ContainerRequests)
	var response pluginapi.AllocateResponse

	for _, req := range r.ContainerRequests {
		var devices []*pluginapi.DeviceSpec
		for _, name := range req.DevicesIDs {
			dev := new(pluginapi.DeviceSpec)

			// There is a possibility the interface already exists from a
			// previous allocation. In a typical scenario, macvtap interfaces
			// would be deleted by the CNI when healthy pod sandbox is
			// terminated. But on occasions, sandbox allocations may fail and
			// the interface is left lingering. The device plugin framework has
			// no de-allocate flow to clean up. So we attempt to delete a
			// possibly existing existing interface before creating it to reset
			// its state.
			var index int
			err := ns.WithNetNSPath(mdp.NetNsPath, func(_ ns.NetNS) error {
				mdp.RLock()
				defer mdp.RUnlock()
				var err error
				glog.Info("create macvtap link ", "deviceName:", name, ",lowerDeviceName:", mdp.LowerDevice, ",mode:", mdp.Mode)
				index, err = util.RecreateMacvtap(name, mdp.LowerDevice, mdp.Mode)
				if err != nil {
					glog.Errorf("create macvtap failed: ", err.Error())
				}
				return err
			})
			if err != nil {
				return nil, err
			}
			// 在宿主机上创建的macvtap设备分配给容器/授予权限
			// 下一步将在容器启动调用cni时将其设备命名空间移动到容器下
			devPath := fmt.Sprint(tapPath, index)
			dev.HostPath = devPath
			dev.ContainerPath = devPath
			dev.Permissions = "rw"
			devices = append(devices, dev)
		}

		response.ContainerResponses = append(response.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			Devices: devices,
		})
	}
	glog.Info("network device allocation successful: ", &response.ContainerResponses)
	return &response, nil
}

func (mdp *macvtapDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return nil, nil
}

func (mdp *macvtapDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		GetPreferredAllocationAvailable: mdp.preferredAllocation,
	}, nil
}

func (mdp *macvtapDevicePlugin) GetPreferredAllocation(_ context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	glog.Info("Into GetPreferredAllocation: ", &req.ContainerRequests)
	response := make([]*pluginapi.ContainerPreferredAllocationResponse, len(req.ContainerRequests))
	resp := &pluginapi.PreferredAllocationResponse{
		ContainerResponses: response,
	}
	for i, request := range req.GetContainerRequests() {
		availableDeviceIDs := request.GetAvailableDeviceIDs()
		glog.V(3).Info("current container[", i, "] request AvailableDeviceIDs: ", availableDeviceIDs)
		glog.V(3).Info("current container[", i, "] request MustIncludeDeviceIDs: ", request.GetMustIncludeDeviceIDs())
		glog.V(3).Info("current container[", i, "] request AllocationSize: ", request.GetAllocationSize())
		sort.Strings(availableDeviceIDs)
		response[i] = &pluginapi.ContainerPreferredAllocationResponse{
			DeviceIDs: availableDeviceIDs[0:request.GetAllocationSize()],
		}
	}
	return resp, nil
}

func (mdp *macvtapDevicePlugin) Stop() error {
	close(mdp.stopWatcher)
	return nil
}
