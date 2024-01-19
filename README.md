# macvtap CNI

This plugin allows users to define Kubernetes networks on top of existing
host interfaces. By using the macvtap plugin, the user is able to directly
connect the pod to a host interface and consume it through a tap device.

The main use cases are virtualization workloads inside the pod driven by
Kubevirt but it can also be used directly with QEMU/libvirt and it might be
suitable combined with other virtualization backends.

macvtap CNI includes a device plugin to properly expose the macvtap interfaces
to the pods. A metaplugin such as [Multus](https://github.com/intel/multus-cni)
gets the name of the interface allocated by the device plugin and is responsible
to invoke the cni plugin with that name as deviceID.

## Deployment

The device plugin is configured through environment variable `DP_MACVTAP_CONF`.
The value is a json array and each element of the array is a separate resource
to be made available:

* `name` (string, required) the name of the resource
* `lowerDevice` (string, required) the name of the macvtap lower link
* `mode` (string, optional, default=bridge) the macvtap operating mode
* `capacity` (uint, optional, default=100) the capacity of the resource

In the default deployment, this configuration shall be provided through a
config map, for [example](examples/macvtap-deviceplugin-config-explicit.yaml):

```yaml
kind: ConfigMap
apiVersion: v1
metadata:
  name: macvtap-deviceplugin-config
data:
  DP_MACVTAP_CONF: |
    [ {
        "name" : "dataplane",
        "lowerDevice" : "eth0",
        "mode": "bridge",
        "capacity" : 50
    } ]
```

```bash
$ kubectl apply -f https://raw.githubusercontent.com/kubevirt/macvtap-cni/main/examples/macvtap-deviceplugin-config.yaml
configmap "macvtap-deviceplugin-config" created
```

当前分支部署方式

```bash
$ kubectl apply -f manifests/macvtap.yaml
```

This configuration will result in up to 50 macvtap interfaces being offered for
consumption, using eth0 as the lower device, in bridge mode, and under
resource name `macvtap.network.kubevirt.io/dataplane`.

A configuration consisting of an empty json array, as proposed in the default
[example](examples/macvtap-deviceplugin-config-default.yaml), causes the device
plugin to expose one resource for every physical link or bond on each node. For
example, if a node has a physical link called eth0, a resourced named
`macvtap.network.kubevirt.io/eth0` would be made available to use macvtap
interfaces with eth0 as the lower device

The macvtap CNI can be deployed using the proposed
[daemon set](manifests/macvtap.yaml):

```
$ kubectl apply -f https://raw.githubusercontent.com/kubevirt/macvtap-cni/main/manifests/macvtap.yaml
daemonset "macvtap-cni" created

$ kubectl get pods
NAME                                 READY     STATUS    RESTARTS   AGE
macvtap-cni-745x4                      1/1    Running           0    5m
```

This will result in the CNI being installed and device plugin running on all
nodes.

There is also a [template](templates/macvtap.yaml.in) available to parameterize
the deployment with different configuration options.

## Usage

macvtap CNI is best used with Multus by defining a NetworkAttachmentDefinition:

```yaml
kind: NetworkAttachmentDefinition
apiVersion: k8s.cni.cncf.io/v1
metadata:
  name: dataplane
  annotations:
    k8s.v1.cni.cncf.io/resourceName: macvtap.network.kubevirt.io/dataplane
spec:
  config: '{
      "cniVersion": "0.3.1",
      "name": "dataplane",
      "type": "macvtap",
      "mtu": 1500
    }'
```

定义multus附加网卡并结合kube-ovn的ipam功能，集中式分配网卡ip和mac

```yaml
kind: NetworkAttachmentDefinition
apiVersion: k8s.cni.cncf.io/v1
metadata:
  name: p4p1
  annotations:
    k8s.v1.cni.cncf.io/resourceName: macvtap.network.kubevirt.io/p4p1
spec:
  config: '{
      "cniVersion": "0.3.1",
      "name": "p4p1",
      "type": "macvtap",
      "mtu": 1500,
      "mode": "bridge",
      "promiscMode": true,
      "ipam": {
        "type": "kube-ovn",
        "server_socket": "/run/openvswitch/kube-ovn-daemon.sock",
        "provider": "p4p1.default"
      }
    }'
```

定义kube-ovn的subnet资源，让kube-ovn的相关注解生效

```yaml
apiVersion: kubeovn.io/v1
kind: Subnet
metadata:
  annotations:
    # 和dcloud2.0的iaas结合，需要说明这个subnet创建的虚拟机所使用的网络模式
    dcloud.tydic.io/interface-type: macvtap
  name: p4p1
spec:
  cidrBlock: 172.31.0.0/24
  default: false
  disableGatewayCheck: true
  excludeIps:
  - 172.31.0.1..172.31.0.40
  gateway: 172.31.0.1
  gatewayNode: ""
  gatewayType: distributed
  natOutgoing: false
  private: false
  protocol: IPv4
  provider: p4p1.default
  vpc: ovn-cluster
```

The CNI config json allows the following parameters:
* `name`     (string, required): the name of the network. Optional when used within a
   NetworkAttachmentDefinition, as Multus provides the name in that case.
* `type`     (string, required): "macvtap".
* `mac`      (string, optional): mac address to assign to the macvtap interface.
* `mtu`      (integer, optional): mtu to set in the macvtap interface.
* `deviceID` (string, required): name of an existing macvtap host interface, which
  will be moved to the correct net namespace and configured. Optional when used within a
  NetworkAttachmentDefinition, as Multus provides the deviceID in that case.
* `promiscMode` (bool, optional): enable promiscous mode on the pod side of the
  veth. Defaults to false.

A pod can be attached to that network which would result in the pod having the corresponding
macvtap interface:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod
  annotations:
    k8s.v1.cni.cncf.io/networks: dataplane
spec:
  containers:
  - name: busybox
    image: busybox
    command: ["/bin/sleep", "1800"]
    resources:
      limits:
        macvtap.network.kubevirt.io/dataplane: 1 
``` 

结合kube-ovn创建的pod 

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod
  annotations:
    k8s.v1.cni.cncf.io/networks: p4p1
    # 指定ovn使用的子网： subnet name
    p4p1.default.kubernetes.io/logical_switch: p4p1
spec:
  containers:
  - name: busybox
    image: busybox
    command: ["/bin/sleep", "1800"]
    resources:
      limits:
        # 指定要分配给pod的macvtap资源
        macvtap.network.kubevirt.io/p4p1: 1 
``` 

结合kube-ovn创建指定ip的pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod
  annotations:
    k8s.v1.cni.cncf.io/networks: default/p4p1
    # 指定ovn使用的子网： subnet name
    p4p1.default.kubernetes.io/logical_switch: p4p1
    p4p1.default.kubernetes.io/ip_address: 172.31.0.10
spec:
  containers:
  - name: busybox
    image: busybox
    command: ["/bin/sleep", "1800"]
    resources:
      limits:
        # 指定要分配给pod的macvtap资源
        macvtap.network.kubevirt.io/p4p1: 1 
``` 

A mac can also be assigned to the interface through the network annotation:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod-with-mac
  annotations:
    k8s.v1.cni.cncf.io/networks: |
      [
        {
          "name":"dataplane",
          "mac": "02:23:45:67:89:01"
        }
      ]
spec:
  containers:
  - name: busybox
    image: busybox
    command: ["/bin/sleep", "1800"]
    resources:
      limits:
        macvtap.network.kubevirt.io/dataplane: 1 
```

结合kube-ovn创建指定网卡mac地址的pod 

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod-with-mac
  annotations:
    # 指定ovn使用的子网： subnet name
    p4p1.default.kubernetes.io/logical_switch: p4p1
    # 可以直接通过ovn注解指定mac地址
    p4p1.default.kubernetes.io/mac_address: "02:23:45:67:89:01"
    # k8s.v1.cni.cncf.io/networks: default/p4p1 
    k8s.v1.cni.cncf.io/networks: |
      [
        {
          "name":"default/p4p1",
          "mac": "02:23:45:67:89:01" # 如果再这里定义了mac地址，要和ovn注解的mac地址保持一致，否则默认以ipam分配的地址为主
        }
      ]
spec:
  containers:
  - name: busybox
    image: busybox
    command: ["/bin/sleep", "1800"]
    resources:
      limits:
        macvtap.network.kubevirt.io/p4p1: 1 
```

结合kube-ovn创建指定网卡ip和mac地址的pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod-with-mac
  annotations:
    # 指定ovn使用的子网： subnet name
    p4p1.default.kubernetes.io/logical_switch: p4p1
    p4p1.default.kubernetes.io/mac_address: "02:23:45:67:89:01"
    p4p1.default.kubernetes.io/ip_address: 172.31.0.10
    k8s.v1.cni.cncf.io/networks: default/p4p1
spec:
  containers:
  - name: busybox
    image: busybox
    command: ["/bin/sleep", "1800"]
    resources:
      limits:
        macvtap.network.kubevirt.io/p4p1: 1 
```

结合kube-ovn创建以multus附加网卡为默认网卡的pod（pod不分配k8s默认pod网络网卡，以macvtap作为主网卡）

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod-with-mac
  annotations:
    # 指定ovn使用的子网： subnet name
    p4p1.default.kubernetes.io/logical_switch: p4p1
    p4p1.default.kubernetes.io/mac_address: "02:23:45:67:89:01"
    p4p1.default.kubernetes.io/ip_address: 172.31.0.10
    # 指定附加网络资源名称
    v1.multus-cni.io/default-network: default/p4p1
spec:
  containers:
  - name: busybox
    image: busybox
    command: ["/bin/sleep", "1800"]
    resources:
      limits:
        macvtap.network.kubevirt.io/p4p1: 1 
```

**Note:** The resource limit can be ommited from the pod definition if 
[network-resources-injector](https://github.com/intel/network-resources-injector)
is deployed in the cluster.

The device plugin can potentially be used by itself in case you only need the
tap device in the pod and not the interface:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: macvtap-consumer
spec:
  containers:
  - name: busybox
    image: busybox
    command: ["/bin/sleep", "123"]
    resources:
      limits:
        macvtap.network.kubevirt.io/dataplane: 1 
```
