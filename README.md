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

Anything the driver cannot see in sysfs, such as which fabric a port reaches, comes from the files in `--attributes-file`, a comma-separated list. The first is the admin file, written by a person. Later files are generated, for example by `fabric-nic-prep`.

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
- When `bootID` is set and differs from `/proc/sys/kernel/random/boot_id`, the whole file is ignored, so attributes written before a reboot are never published. A generated file must carry a `bootID`. An admin file should leave it out: what a person writes, such as which fabric a port reaches, holds across reboots, and an admin file with a stale `bootID` is unusable, which also stops `fabric-nic-prep` from binding.
- An earlier file wins for a whole device: once a file names a device, later files are not read for it.
- `roleSource` is `file` for a device the admin file names, `auto` for one only a generated file names, and `none` otherwise.
- Attribute names and values must pass the ResourceSlice rules (a DNS subdomain, a C identifier of at most 32 characters, strings of at most 64 bytes, semantic versions); a file with one bad entry is rejected as a whole.
- The driver rereads the files and sysfs every `--rescan-interval` (30s) and republishes on a change. A file that fails to load is skipped as a whole and logged.

`deploy/deviceclasses.yaml` ships two kinds of compute class. `ib-pf-compute` and `ib-pf-compute-guest` accept only `roleSource == 'file'`. `ib-pf-compute-auto` and `ib-pf-compute-auto-guest` also accept `auto`, a role that `fabric-nic-prep` inferred and that can be wrong.

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

`fabric-nic-prep` is an optional, privileged node component, deployed from `deploy/prep.yaml` as its own DaemonSet. It reads fabric facts from InfiniBand ports while they are still on their host driver, infers which ports belong to the compute fabric, writes that as a generated attribute file, and at the bind level hands those ports to vfio-pci. The plugin does not know it exists: it only reads the files.

The node label `fabric-nic.inerplat.github.io/prep` selects what prep does on a node:

| Label | Prep |
|---|---|
| absent or `disabled` | not scheduled |
| `report` | classifies and writes `report.json` and `attributes.proposed.json`, which the plugin does not read; binds nothing |
| `bind` | classifies; on a match writes `attributes.generated.json` and binds the eligible compute ports to vfio-pci |

Changing `bind` to `report` or `disabled` stops new binds. Prep never moves anything off vfio-pci, so ports it bound stay there until a reboot or until an operator returns them.

### Inferred roles can be wrong

Prep recognises one topology, an HGX-style board, and only marks ports `example.com/fabric=compute`. It needs all of:

1. Every InfiniBand port on the node is Active, has a subnet prefix, and answered the subnet manager query. A port anywhere with unknown facts might belong to the GPU fabric, so the pass is incomplete. A function with more than one port is not classified at all.
2. Exactly one fabric, identified by subnet prefix and master SM GUID, gives every GPU root exactly one port.
3. That fabric has no port outside a GPU root, and every GPU root holds one GPU.

A GPU is a 3D controller (class 0x0302), or a display-class device from a vendor in `--gpu-vendors` (default `10de`). Other display devices, such as a BMC's VGA, are listed in the report as excluded.

The facts are read twice per pass, so a subnet manager failover during the pass makes it incomplete rather than wrong. An incomplete pass (a port down, a query refused, a change during the pass) is retried with backoff up to `--retry-max`. An unmatched one is retried only when prep restarts. A match is kept for the rest of the boot.

Prep refuses, with the failing clause in the report, on boards it does not recognise:
- a NIC shared by two GPUs, or several GPUs under one root;
- GPUs attached to the CPU over a coherent link rather than PCIe, with no NIC on their root;
- fewer NICs than GPUs;
- one subnet per rail;
- a fabric whose subnet manager refuses queries from hosts;
- a node that binds its NICs to vfio-pci before prep runs.

It also cannot tell a converged compute and storage fabric from a compute fabric, so do not run the bind level there. Storage and every other role come only from the admin file. Prep publishes no rail, switch or fabric identity.

### Binding

At the bind level prep moves a compute port to vfio-pci only when all of these hold:
- the generated file naming it is in place;
- the admin file gives it no other role;
- the port has no VFs;
- prep has not bound it before in this boot;
- its IOMMU group holds nothing else but bridges;
- the host does not use it, checked again right before the bind.

The host uses a port when it, or anything stacked on it (P_Key and VLAN children, bonds and bridges, found through the kernel's links rather than names), has an address other than link-local, is a bond or bridge member, carries an IPv4 or IPv6 default route, or is used by LNet. It also counts as used when a process holds a queue pair on the device or holds one of its umad, issm or uverbs character devices, compared by device number. A check that cannot run counts as use. An admin file that exists but does not load stops all binding until it is fixed.

To undo or prevent a bind without a reboot:
- Return the port to its host driver by hand; prep records it as `returned-by-operator` and leaves it for the rest of the boot.
- Set `example.com/fabric` to `host` for it in the admin file; this lasts across reboots.
- Remove the node label.

### Static setup with prep's help

On a node prep cannot classify, or where you would rather decide by hand, run prep at the `report` level, read `report.json` (ports, subnet prefixes, SM GUIDs, neighbor switches, GPU roots), and write the admin file yourself, starting from `attributes.proposed.json` when there is one. Bind the ports with the OS, for example `driverctl set-override <bdf> vfio-pci`.

## Deploy

```bash
kubectl apply -f deploy/rbac.yaml -f deploy/deviceclasses.yaml
kubectl label node <node> fabric-nic.inerplat.github.io/enabled=true
kubectl apply -f deploy/daemonset.yaml
# optional
kubectl apply -f deploy/prep.yaml
kubectl label node <node> fabric-nic.inerplat.github.io/prep=report
```

The plugin DaemonSet needs the kubelet plugin directories, `/var/run/cdi` and the attribute file directory `/var/lib/fabric-nic`, and reads `/sys` and `/proc` as the container sees them. It is not privileged; it runs as root for its writes to the host paths. Prep is privileged and runs in the host network and PID namespaces; its image needs `sminfo` and `smpquery` (infiniband-diags) and `rdma` (iproute2).

## License

Copyright The fabric-nic Authors. Licensed under the Apache License, Version 2.0; see [LICENSE](LICENSE).
