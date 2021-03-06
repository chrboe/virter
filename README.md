# Virter

Virter is a command line tool for simple creation and cloning of virtual
machines.

Virter supports VMs running standard general purpose distributions such as
CentOS and Ubuntu. It is especially useful for development and testing of
projects which cannot use containers due to kernel dependencies, such as
[DRBD](https://github.com/LINBIT/drbd) and
[LINSTOR](https://github.com/LINBIT/linstor-server).

## Quick Start

First install and set up [libvirt](https://libvirt.org/index.html). Then
download one of the [releases](https://github.com/LINBIT/virter/releases).
Virter is packaged as a single binary, so just put that into `/usr/local/bin` and
you are ready to use virter:

```
virter image pull centos-7 # also would be auto-pulled in next step
virter vm run --name centos-7-hello --id 100 --wait-ssh centos-7
virter vm ssh centos-7-hello
virter vm rm centos-7-hello
```

## Building from source

If you want to test the latest unstable version of virter, you can build the
git version from sources:

```
git clone https://github.com/LINBIT/virter
cd virter
go build .
```

## Installation Details

Virter requires:

* A running libvirt daemon on the host where it is run

Configuration is read by default from `~/.config/virter/virter.toml`.

When starting virter for the first time, a default configuration file will be
generated, including documentation about the various flags.

### Network domain

If you require DNS resolution from your VMs to return correct FQDNs, add the
`domain` to your libvirt network definition:

```
<network>
  ...
  <domain name='test'/>
  ...
</network>
```

By default, virter uses the libvirt network named `default`.

### DHCP Leases

Libvirt produces some weird behavior when MAC or IP addresses are reused while
there is still an active DHCP lease for them. This can result in a new VM
getting assigned a random IP instead of the IP corresponding to its ID.

To work around this, virter tries to execute the `dhcp_release` utility in
order to release the DHCP lease from libvirt's DHCP server when a VM is
removed. This utility has to be run by the root user, so virter executes
it using `sudo`.

If execution fails (for example because the utility is not installed or the
sudo rules are not set up correctly), the error is ignored by virter.

So, to make virter work more reliably, especially when you are running lots
of VMs in a short amount of time, you should install the `dhcp_release` utility
(usually packaged as `dnsmasq-utils`). Additionally, you should make sure that
your user can run `dhcp_release` as root, for example by using a sudo rule like
this:

```
%libvirt ALL=(ALL) NOPASSWD: /usr/bin/dhcp_release
```

This allows all users in the group libvirt to run the `dhcp_release` utility
without being prompted for a password.

## Usage

For usage just run `virter help`.

## Architecture

Virter connects to the libvirt daemon for all the heavy lifting. It supplies
bootstrap configuration to the VMs using `cloud-init` volumes, so that the
hostname is set and SSH access is possible.

## Comparison to other tools

### `virsh`

Virter is good for starting and cloning `cloud-init` based VMs. `virsh` is
useful for more detailed libvirt management. They work well together.

### `virt-install`

`virt-install` is built for the images that use conventional installers. Virter
uses `cloud-init`, making it simpler to use and quicker to start a fresh VM.

### Running VMs in AWS/GCP/OpenNebula

Virter is local to a single host making snapshot/restore/clone operations very
efficient. Virter could be thought of as cloud provisioning for your local
machine.

### Vagrant

Virter and Vagrant have essentially the same goal. Virter is more tightly
integrated with the Linux virtualization stack, resulting in better
snapshot/restore/clone support.

### Multipass

Virter and Multipass have similar goals, but Multipass is Ubuntu specific.

### Docker

Virter is like Docker for VMs. The user experience of the tools is generally
similar. Docker containers share the host kernel, whereas Virter starts VMs
with their own kernel.

### Kata Containers

Virter starts VMs running a variety of Linux distributions, whereas Kata
Containers uses a specific guest that then runs containers.

### Weave Ignite

Ignite has very strong requirements on the guest, so it cannot be used for
running standard distributions.

## Development

Virter is a standard go project using modules.
Go 1.13+ is supported.
