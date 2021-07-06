# NVIDIA Driver Manager For Kubernetes
The NVIDIA Driver Manager is a Kubernetes component which assist in seamless upgrades of NVIDIA Driver on each node of the cluster. This component ensure that all pre-requisites are met before driver upgrades can be performed using [NVIDIA GPU Driver](https://ngc.nvidia.com/containers/nvstaging:cloud-native:driver). Following are the actions performed by this component when upgrade is required.

1. Check for already installed kernel modules.
2. Perform Drain on the node ignoring Daemonset pods.
3. Evict GPU Operator components like Device-Plugin, GPU Feature Discovery, DCGM Exporter etc.
4. Unload kernel-modules.
5. Unmount Driver root filesystem mounted on the host previously under /run/nvidia/driver.
6. Uncordon the node.

These steps allows new versions can be easily installed in the Kubernetes cluster.
