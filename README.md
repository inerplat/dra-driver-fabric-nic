# dra-driver-vfio-nic

A DRA driver that publishes the network functions a node has bound to vfio-pci, for VM runtimes such as KubeVirt and Kata. It only publishes and names devices: it does not create VFs, bind drivers or decide what a function is for.

Pair a NIC with a GPU from another driver, such as the NVIDIA GPU DRA driver, by matching a standard attribute in the claim (`examples/claim-gpu4-nic4.yaml`).

## Devices

One device per PCI function whose class is a network controller and whose driver is vfio-pci or a vendor variant of it (`mlx5_vfio_pci`). A physical function that still has VFs enabled is skipped. The device name is the PCI address with `:` and `.` replaced by `-`.

| Attribute | Value |
|---|---|
| `function` | `pf` or `vf` |
| `linkType` | `infiniband`, `ethernet` or `other`, from the PCI subclass |
| `vendor`, `device` | PCI IDs, four hex digits |
| `parentPF` | PCI address of the physical function, VFs only |
| `resource.kubernetes.io/pciBusID` | PCI address |
| `resource.kubernetes.io/pcieRoot` | PCIe root complex |
| `resource.kubernetes.io/numaNode` | NUMA node, when the platform reports one |

Attributes without a domain are in the driver's domain, `vfio-nic.inerplat.github.io`.

## Attributes from a file

Anything the driver cannot see in sysfs, such as which fabric a port reaches, comes from `--attributes-file`, written by another component on the node:

```json
{
  "version": "v1",
  "bootID": "0f3c6a9e-5d21-4b8e-9a77-2c1d4e8b6f10",
  "devices": {
    "0000:9c:00.0": {
      "example.com/fabric": {
        "string": "compute"
      }
    }
  }
}
```

Every attribute needs a domain, and the driver's domain and `resource.kubernetes.io` are refused. When `bootID` is set and differs from `/proc/sys/kernel/random/boot_id`, the whole file is ignored, so attributes written before a reboot are never published. A DeviceClass that requires such an attribute therefore matches nothing until the file is rewritten. The driver rereads the file and sysfs every `--rescan-interval` (30s) and republishes on a change.

## VFIO backends

The CDI spec (`/var/run/cdi/vfio-nic.inerplat.github.io_nic.json`) lists each device twice, once per backend:

| CDI device | Device nodes |
|---|---|
| `<name>-legacy` | `/dev/vfio/<group>`, `/dev/vfio/vfio` |
| `<name>-iommufd` | `/dev/vfio/devices/vfioN`, `/dev/iommu` (only with a VFIO cdev) |

A VM runtime attaches one device per node it is given, so a claim gets exactly one backend: `--vfio-backend` (default `legacy`), overridden by a `NicConfig` in the class or claim:

```yaml
config:
- requests:
  - nic0
  opaque:
    driver: vfio-nic.inerplat.github.io
    parameters:
      apiVersion: vfio-nic.inerplat.github.io/v1alpha1
      kind: NicConfig
      vfioBackend: iommufd
```

Prepare writes no files of its own. It returns the CDI device and KEP-5304 device metadata with `resource.kubernetes.io/pciBusID`, which KubeVirt reads to assign the device. Unprepare has nothing to undo.

## Deploy

```bash
kubectl apply -f deploy/rbac.yaml -f deploy/deviceclasses.yaml
kubectl label node <node> vfio-nic.inerplat.github.io/enabled=true
kubectl apply -f deploy/daemonset.yaml
```

The DaemonSet needs the kubelet plugin directories, `/var/run/cdi` and the attribute file directory `/var/lib/vfio-nic`. It reads `/sys` and `/proc` as the container sees them.
