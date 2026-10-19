# dra-driver-fabric-nic

A DRA driver that publishes the network functions a node has bound to vfio-pci, for runtimes that pass them to a VM. It only publishes and names devices: it does not create VFs, bind drivers or decide what a function is for.

Pair a NIC with a GPU from another driver, such as the NVIDIA GPU DRA driver, by matching a standard attribute in the claim (`examples/claim-gpu4-nic4.yaml`).

## Devices

One device per PCI function whose class is a network controller and whose driver is vfio-pci or a vendor variant of it (`mlx5_vfio_pci`). A physical function that still has VFs enabled is skipped. So is a function whose IOMMU group holds any other device that is not a bridge: a VM given the group's VFIO node gets every member, so that device would be handed out with it. The device name is the PCI address with `:` and `.` replaced by `-`.

| Attribute | Value |
|---|---|
| `function` | `pf` or `vf` |
| `linkType` | `infiniband`, `ethernet` or `other`, from the PCI subclass |
| `vendor`, `device` | PCI IDs, four hex digits |
| `parentPF` | PCI address of the physical function, VFs only |
| `resource.kubernetes.io/pciBusID` | PCI address |
| `resource.kubernetes.io/pcieRoot` | PCIe root complex |
| `resource.kubernetes.io/numaNode` | NUMA node, when the platform reports one |
| `roleSource` | `file`, `auto` or `none`: which attribute file the device's entry came from |

Attributes without a domain are in the driver's domain, `fabric-nic.inerplat.github.io`.

## Attributes from files

Anything the driver cannot see in sysfs, such as which fabric a port reaches, comes from the files in `--attributes-file`, a comma-separated list. The first holds roles a person declared, the later ones roles a tool detected. `fabric-nic-prep` writes both kinds from the node's port annotations and its own detection; another tool or a person can write them instead.

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

- Every attribute needs a domain. The driver's domain and `resource.kubernetes.io` are refused.
- When `bootID` is set and differs from `/proc/sys/kernel/random/boot_id`, the whole file is ignored, so attributes written before a reboot are never published. Every file after the first must carry a `bootID`. `fabric-nic-prep` writes a `bootID` into both.
- An earlier file wins for a whole device: once a file names a device, later files are not read for it.
- `roleSource` is `file` for a device the first file names, `auto` for one only a later file names, and `none` otherwise.
- Attribute names and values must pass the ResourceSlice rules (a DNS subdomain, a C identifier of at most 32 characters, strings of at most 64 bytes, semantic versions); a file with one bad entry is rejected as a whole.
- The driver rereads the files and sysfs every `--rescan-interval` (30s) and republishes on a change. A file that fails to load is skipped as a whole and logged.

`deploy/deviceclasses.yaml` ships two kinds of compute class. `ib-pf-compute` and `ib-pf-compute-guest` accept only `roleSource == 'file'`. `ib-pf-compute-auto` and `ib-pf-compute-auto-guest` also accept `auto`, a role that `fabric-nic-prep` detected and that can be wrong.

## VFIO backends

The CDI spec (`/var/run/cdi/fabric-nic.inerplat.github.io_nic.json`) lists each device twice, once per backend:

| CDI device | Device nodes |
|---|---|
| `<name>-legacy` | `/dev/vfio/<group>`, `/dev/vfio/vfio` |
| `<name>-iommufd` | `/dev/vfio/devices/vfioN`, `/dev/iommu` (only with a VFIO cdev) |

A VM runtime attaches one device per node it is given, so a claim gets exactly one backend: `--vfio-backend` (default `legacy`), overridden by a `NicConfig` in the class or claim. A claim config overrides a class config, and a config naming requests overrides one that names none. A config for a request also applies to its first-available subrequests (`nic/fast`):

```yaml
config:
- requests:
  - nic0
  opaque:
    driver: fabric-nic.inerplat.github.io
    parameters:
      apiVersion: fabric-nic.inerplat.github.io/v1alpha1
      kind: NicConfig
      vfioBackend: iommufd
```

## RDMA verbs nodes in a VM guest

A runtime that runs the container in a VM creates the container's device nodes inside the guest from the OCI spec. An InfiniBand function passed through to such a guest gets its verbs node, `/dev/infiniband/uverbsN`, from the guest kernel, and the container needs that node to use RDMA.

With `guestRDMA: true` in a `NicConfig`, a claim with at least one InfiniBand function also gets the CDI device `guest-rdma`. It lists `/dev/infiniband/uverbs0` to `uverbs31` with the char numbers ib_uverbs gives its first 32 devices (major 231, minors 192 to 223) and no host path. The guest numbers its devices in its own probe order, which the driver cannot see, so the device lists all of them; verbs libraries open only the nodes that sysfs names.

On a container that shares the host kernel the same numbers are the host's RDMA devices, so only a DeviceClass may set `guestRDMA`; a claim that sets it fails to prepare. `deploy/deviceclasses.yaml` has an example class.

Prepare writes no files of its own. It returns the CDI device and KEP-5304 device metadata with `resource.kubernetes.io/pciBusID`, which a VM runtime that reads KEP-5304 device metadata uses to assign the device. Unprepare has nothing to undo.

## fabric-nic-prep

`fabric-nic-prep` is an init container of the plugin pod. It runs once per pod start, decides the role of every Mellanox network PF on the node, binds the ones meant for vfio-pci, writes the plugin's attribute files and `report.json` under `/var/lib/fabric-nic`, prints a table of what it saw, and exits. It reads the Node but never writes it.

The init container is privileged. The pod keeps its own network and PID namespaces: prep reads the Node from the pod, then runs the rest of itself in the host's network namespace through `nsenter`, and reads the host's processes through the host's `/proc`, mounted at `/host/proc` beside the host's `/sys` at `/host/sys`. A PID namespace cannot be entered from inside a child of it, so prep does not try. Before binding it checks that it sees the host (it is in the network namespace of the host's PID 1, `/host/proc` shows it, and netlink and sysfs list the same interfaces); when the check fails it binds nothing.

### Ports

A port is identified by its VPD serial number and PCI function, `<serial>-f<function>`, which follow the card rather than the slot. A function whose VPD cannot be read, has no usable serial, or shares its ID with another function has no ID and gets no role. A card whose VPD carries `SW_MNG` is a ConnectX bridge that manages NVLink switches; NVIDIA's Fabric Manager start script uses the same marker.

### Node keys

| Key | Kind | Meaning | Values |
|---|---|---|---|
| `fabric-nic.inerplat.github.io/mode` | label | what prep does on the node | `auto`, `explicit`, `report`, `off`; without the label, `DEFAULT_MODE` of the DaemonSet (`report`) |
| `fabric-nic.inerplat.github.io/port.<serial>-f<function>` | annotation, one per port | a declared role and whether prep binds the port | `<compute\|storage\|other>/<vfio-pci\|keep>` |

`vfio-pci` binds the port at every pod start; the binding does not survive a reboot, so prep binds again after one. `keep` leaves the port on whatever driver it has, which is the value for a port someone else has pinned to vfio-pci. Prep never moves a port off vfio-pci.

| Mode | Declared ports | Other ports | Binds |
|---|---|---|---|
| `auto` | as declared (`roleSource=file`) | detected compute ports (`roleSource=auto`) | yes |
| `explicit` | as declared | left alone and given no role | yes |
| `report` | previewed | detection previewed | no |
| `off` | not read | not read | no |

A declared role is published even when the port cannot be bound, unless the hardware rules it out: an NVLink management bridge can only be `other`, and a `compute` port must share a PCIe root with a GPU. A mode value outside the list, or a malformed port annotation on an `auto` or `explicit` node, makes prep bind nothing and publish no roles until it is fixed. An annotation naming a port the node does not have is reported as stale. `deploy/validation.yaml` rejects malformed keys and values when they are written, and keeps kubelets from setting any of them except `enabled`.

Changes take effect at the next pod start: delete the node's plugin pod after changing the label or annotations.

### Declaring ports

Run the node in `report` mode and read the init container's log, `kubectl -n fabric-nic logs <pod> -c prep`. It shows one row per port (ID, PCI address, driver, PCIe root, the GPU on that root, the NVLink management marker, fabric, role, result) and the commands that declare what it saw: values it detected are filled in, the rest are listed as comments to fill in. Review the values, run the commands, and delete the pod. `report.json` holds the same and every fact behind it.

### Detection

In `auto` mode prep detects the compute ports at the first pod start of every boot, since the hardware may have changed while the node was down, and keeps the match for the rest of the boot. It recognises one topology, an HGX-style board:

1. NVLink management bridges are left out.
2. Every other InfiniBand port is Active, has a subnet prefix and answered the subnet manager query, and every function has an ID and a known PCIe root. Otherwise the detection is incomplete.
3. Every other InfiniBand PF was probed on its host driver, unless prep bound it in this boot: the fabric of any other port is unknown.
4. Exactly one fabric, identified by subnet prefix and master SM GUID, gives every GPU root exactly one port; that fabric has no port outside a GPU root; every GPU root holds one GPU.

The facts are read twice, so a subnet manager failover during the run makes it incomplete rather than wrong. An incomplete detection within `--deadline` (10 minutes) of boot makes prep exit 1, so the kubelet runs it again; after that prep lets the plugin start without detected roles. A GPU is a 3D controller (class 0x0302), or a display-class device from a vendor in `--gpu-vendors` (default `10de`).

Detected roles can be wrong. A match proves the shape of the fabric, not its purpose: when the real compute ports are missing or invisible and another fabric happens to give every GPU root one port, prep binds that fabric's ports. Host use the checks below cannot see, such as a kernel RDMA consumer with no file descriptor or address, or one that starts after prep, is not caught either. Use `report` mode on a new kind of node before `auto`, and the strict DeviceClasses where a wrong NIC matters. Every other unrecognised shape binds nothing, with the reason in the report: a NIC shared by two GPUs or several GPUs under one root, fewer NICs than GPUs, two NICs per GPU root, one subnet per rail, a subnet manager that refuses host queries, an unmarked NVLink bridge that does not answer, ports pinned to vfio-pci before prep ran.

### Binding

Prep binds a port to vfio-pci only when all of these hold:
- it is declared, or detected, with `vfio-pci`, and has an ID;
- it is not an NVLink management bridge;
- a `compute` port shares a PCIe root with a GPU;
- the port has no VFs, and its IOMMU group holds nothing else but bridges;
- prep has not bound it before in this boot; one an operator returned to its host driver is left there for the rest of the boot;
- its driver shows an RDMA device (InfiniBand) or a network interface (Ethernet) in the host's namespaces, since one moved to another namespace cannot be checked;
- the host does not use it, checked again right before the bind, and the card in the slot is still the declared one, checked by its VPD right before it is detached.

The host uses a port when it, or anything stacked on it (P_Key and VLAN children, bonds and bridges, found through the kernel's links rather than names), has an address other than link-local, is a bond or bridge member, carries an IPv4 or IPv6 default route, or is used by LNet (the host's `lnetctl`, run in the host's root). It also counts as used when a process holds one of the device's umad, issm or uverbs character devices, compared by device number, which every user queue pair needs; when prep runs in the host's PID namespace it also asks the kernel for queue pairs owned by processes. A check that cannot run counts as use.

### Upgrading from the prep DaemonSet

Earlier versions ran prep as its own DaemonSet with a `prep=report|bind` node label and read a hand-written `attributes.json`. Removing that manifest from the repository does not remove the objects, and the old DaemonSet would race the init container over the same files. Before applying the new `deploy/daemonset.yaml`:

```bash
kubectl -n fabric-nic delete daemonset fabric-nic-prep
kubectl -n fabric-nic delete serviceaccount fabric-nic-prep
kubectl delete clusterrolebinding fabric-nic-prep
kubectl delete clusterrole fabric-nic-prep
kubectl label nodes --all fabric-nic.inerplat.github.io/prep-
```

prep now owns `attributes.json` and rewrites it at every pod start. Carry its entries over as port annotations: start the node in `report` mode and use the IDs from the log.

## License

Copyright The fabric-nic Authors. Licensed under the Apache License, Version 2.0; see [LICENSE](LICENSE).
