package virter

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/LINBIT/virter/pkg/netcopy"
	libvirtxml "github.com/libvirt/libvirt-go-xml"
	"github.com/rck/unit"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"

	sshclient "github.com/LINBIT/gosshclient"
	"github.com/LINBIT/virter/pkg/actualtime"

	libvirt "github.com/digitalocean/go-libvirt"
)

// ImageExists checks whether an image called imageName exists in the libvirt
// virter storage pool.
func (v *Virter) ImageExists(imageName string) (bool, error) {
	sp, err := v.libvirt.StoragePoolLookupByName(v.storagePoolName)
	if err != nil {
		return false, fmt.Errorf("could not get storage pool: %w", err)
	}

	_, err = v.libvirt.StorageVolLookupByName(sp, imageName)
	if err != nil {
		if hasErrorCode(err, errNoStorageVol) {
			return false, nil
		}
		return false, fmt.Errorf("could not get backing image volume: %w", err)
	}

	return true, nil
}

func (v *Virter) anyImageExists(vmConfig VMConfig) (bool, error) {
	vmName := vmConfig.Name
	imgs := []string{
		vmName,
		ciDataVolumeName(vmName),
	}

	for _, d := range vmConfig.Disks {
		imgs = append(imgs, diskVolumeName(vmName, d.GetName()))
	}

	for _, img := range imgs {
		if exists, err := v.ImageExists(img); exists || err != nil {
			return exists, err
		}
	}
	return false, nil
}

// VMRun starts a VM.
func (v *Virter) VMRun(shellClientBuilder ShellClientBuilder, vmConfig VMConfig) error {
	// checks
	vmConfig, err := CheckVMConfig(vmConfig)
	if err != nil {
		return err
	}

	vmName := vmConfig.Name
	_, err = v.libvirt.DomainLookupByName(vmName)
	if !hasErrorCode(err, errNoDomain) {
		if err != nil {
			return fmt.Errorf("could not get domain: %w", err)
		}
		return fmt.Errorf("domain '%s' already defined", vmName)
	}

	if exists, err := v.anyImageExists(vmConfig); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("one of the images already exists")
	}

	id, err := v.getVMID(vmConfig.ID)
	if err != nil {
		return err
	}
	vmConfig.ID = id
	// end checks

	sp, err := v.libvirt.StoragePoolLookupByName(v.storagePoolName)
	if err != nil {
		return fmt.Errorf("could not get storage pool: %w", err)
	}

	log.Print("Create boot volume")
	err = v.createVMVolume(sp, vmConfig)
	if err != nil {
		return err
	}

	log.Print("Create cloud-init volume")
	err = v.createCIData(sp, vmConfig)
	if err != nil {
		return err
	}

	for _, d := range vmConfig.Disks {
		log.Printf("Create volume '%s'", d.GetName())
		err = v.createDiskVolume(sp, vmConfig.Name, d)
		if err != nil {
			return err
		}
	}

	ip, err := v.createVM(sp, vmConfig)
	if err != nil {
		return err
	}

	if vmConfig.WaitSSH {
		err := pingSSH(shellClientBuilder, vmConfig, ip)
		if err != nil {
			return err
		}
	}

	return nil
}

func (v *Virter) createVMVolume(sp libvirt.StoragePool, vmConfig VMConfig) error {
	imageName := vmConfig.ImageName
	vmName := vmConfig.Name

	backingVolume, err := v.libvirt.StorageVolLookupByName(sp, imageName)
	if err != nil {
		return fmt.Errorf("could not get backing image volume: %w", err)
	}

	backingPath, err := v.libvirt.StorageVolGetPath(backingVolume)
	if err != nil {
		return fmt.Errorf("could not get backing image path: %w", err)
	}

	sizeB := vmConfig.BootCapacityKiB * uint64(unit.K) // user defined one, might be 0 for don't care
	if sizeB == 0 {
		_, sizeB, _, err = v.libvirt.StorageVolGetInfo(backingVolume)
		if err != nil {
			return fmt.Errorf("could not get backing image info: %w", err)
		}
		minSize := uint64(10 * unit.G)
		if sizeB < minSize {
			sizeB = minSize
		}
	}

	xml, err := v.vmVolumeXML(vmName, backingPath, sizeB)
	if err != nil {
		return err
	}

	_, err = v.libvirt.StorageVolCreateXML(sp, xml, 0)
	if err != nil {
		return fmt.Errorf("could not create VM boot volume: %w", err)
	}

	return nil
}

func (v *Virter) createDiskVolume(sp libvirt.StoragePool, vmName string, disk Disk) error {
	xml, err := v.diskVolumeXML(diskVolumeName(vmName, disk.GetName()), disk.GetSizeKiB(), "KiB", disk.GetFormat())
	if err != nil {
		return err
	}

	_, err = v.libvirt.StorageVolCreateXML(sp, xml, 0)
	if err != nil {
		return fmt.Errorf("could not create scratch volume: %w", err)
	}

	return nil
}

func diskVolumeName(vmName string, diskName string) string {
	return vmName + "-" + diskName
}

func (v *Virter) createVM(sp libvirt.StoragePool, vmConfig VMConfig) (net.IP, error) {
	xml, err := v.vmXML(sp.Name, vmConfig)
	if err != nil {
		return nil, err
	}

	log.Debugf("Using domain XML: %s", xml)

	log.Print("Define VM")
	d, err := v.libvirt.DomainDefineXML(xml)
	if err != nil {
		return nil, fmt.Errorf("could not define domain: %w", err)
	}

	domainXML, err := v.libvirt.DomainGetXMLDesc(d, 0)
	if err != nil {
		return nil, err
	}

	domcfg := &libvirtxml.Domain{}
	err = domcfg.Unmarshal(domainXML)
	if err != nil {
		return nil, err
	}

	mac := domcfg.Devices.Interfaces[0].MAC.Address

	// Add DHCP entry after defining the VM to ensure that it can be
	// removed when removing the VM, but before starting it to ensure that
	// it gets the correct IP address
	ip, err := v.addDHCPEntry(mac, vmConfig.ID)
	if err != nil {
		return nil, err
	}

	log.Print("Start VM")
	err = v.libvirt.DomainCreate(d)
	if err != nil {
		return nil, fmt.Errorf("could not create (start) domain: %w", err)
	}

	return ip, nil
}

func pingSSH(shellClientBuilder ShellClientBuilder, vmConfig VMConfig, ip net.IP) error {
	log.Print("Wait for SSH port to open")

	hostPort := net.JoinHostPort(ip.String(), "ssh")

	sshConfig, err := getSSHClientConfig(vmConfig.SSHPrivateKey)
	if err != nil {
		return err
	}
	sshConfig.Timeout = vmConfig.SSHPingPeriod

	sshTry := func() error {
		return tryDialSSH(shellClientBuilder, hostPort, sshConfig)
	}

	// Using ActualTime breaks the expectation of the unit tests
	// that this code does not sleep, but we work around that by
	// always making the first ping successful in tests
	if err := (actualtime.ActualTime{}.Ping(vmConfig.SSHPingCount, vmConfig.SSHPingPeriod, sshTry)); err != nil {
		return fmt.Errorf("unable to connect to SSH port: %w", err)
	}

	log.Print("Successfully connected to SSH port")
	return nil
}

func tryDialSSH(shellClientBuilder ShellClientBuilder, hostPort string, sshConfig ssh.ClientConfig) error {
	sshClient := shellClientBuilder.NewShellClient(hostPort, sshConfig)
	if err := sshClient.Dial(); err != nil {
		log.Debugf("SSH dial attempt failed: %v", err)
		return err
	}
	sshClient.Close()
	return nil
}

// VMRm removes a VM.
func (v *Virter) VMRm(vmName string) error {
	sp, err := v.libvirt.StoragePoolLookupByName(v.storagePoolName)
	if err != nil {
		return fmt.Errorf("could not get storage pool: %w", err)
	}

	err = v.vmRmExceptBoot(sp, vmName)
	if err != nil {
		return err
	}

	err = v.rmVolume(sp, vmName, "boot")
	if err != nil {
		return err
	}

	return nil
}

func (v *Virter) vmRmExceptBoot(sp libvirt.StoragePool, vmName string) error {
	domain, err := v.libvirt.DomainLookupByName(vmName)
	if !hasErrorCode(err, errNoDomain) {
		if err != nil {
			return fmt.Errorf("could not get domain: %w", err)
		}

		disks, err := v.getDisksOfDomain(domain)
		if err != nil {
			return err
		}

		err = v.rmSnapshots(domain)
		if err != nil {
			return err
		}

		active, err := v.libvirt.DomainIsActive(domain)
		if err != nil {
			return fmt.Errorf("could not check if domain is active: %w", err)
		}

		persistent, err := v.libvirt.DomainIsPersistent(domain)
		if err != nil {
			return fmt.Errorf("could not check if domain is persistent: %w", err)
		}

		err = v.rmDHCPEntry(domain)
		if err != nil {
			return err
		}

		if active != 0 {
			log.Print("Stop VM")
			err = v.libvirt.DomainDestroy(domain)
			if err != nil {
				return fmt.Errorf("could not destroy domain: %w", err)
			}
		}

		if persistent != 0 {
			log.Print("Undefine VM")
			err = v.libvirt.DomainUndefine(domain)
			if err != nil {
				return fmt.Errorf("could not undefine domain: %w", err)
			}
		}

		for _, disk := range disks {
			if disk == vmName {
				// do not delete boot volume
				continue
			}
			err = v.rmVolume(sp, disk, "disk")
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (v *Virter) rmSnapshots(domain libvirt.Domain) error {
	snapshots, _, err := v.libvirt.DomainListAllSnapshots(domain, -1, 0)
	if err != nil {
		return fmt.Errorf("could not list snapshots: %w", err)
	}

	for _, snapshot := range snapshots {
		log.Printf("Delete snapshot %v", snapshot.Name)
		err = v.libvirt.DomainSnapshotDelete(snapshot, 0)
		if err != nil {
			return fmt.Errorf("could not delete snapshot: %w", err)
		}
	}

	return nil
}

func (v *Virter) rmVolume(sp libvirt.StoragePool, volumeName string, debugName string) error {
	volume, err := v.libvirt.StorageVolLookupByName(sp, volumeName)
	if !hasErrorCode(err, errNoStorageVol) {
		if err != nil {
			return fmt.Errorf("could not get %v volume: %w", debugName, err)
		}

		log.Printf("Delete %v volume", debugName)
		err = v.libvirt.StorageVolDelete(volume, 0)
		if err != nil {
			return fmt.Errorf("could not delete %v volume: %w", debugName, err)
		}
	}

	return nil
}

// VMCommit commits a VM to an image. If shutdown is true, a goroutine to watch
// for events will be started. This goroutine will only terminate when the
// libvirt connection is closed, so take care of leaking goroutines.
func (v *Virter) VMCommit(afterNotifier AfterNotifier, vmName string, shutdown bool, shutdownTimeout time.Duration) error {
	domain, err := v.libvirt.DomainLookupByName(vmName)
	if err != nil {
		return fmt.Errorf("could not get domain: %w", err)
	}

	if shutdown {
		err = v.vmShutdown(afterNotifier, shutdownTimeout, domain)
		if err != nil {
			return err
		}
	} else {
		active, err := v.libvirt.DomainIsActive(domain)
		if err != nil {
			return fmt.Errorf("could not check if domain is active: %w", err)
		}

		if active != 0 {
			return fmt.Errorf("cannot commit a running VM")
		}
	}

	sp, err := v.libvirt.StoragePoolLookupByName(v.storagePoolName)
	if err != nil {
		return fmt.Errorf("could not get storage pool: %w", err)
	}

	err = v.vmRmExceptBoot(sp, vmName)
	if err != nil {
		return err
	}

	return nil
}

func (v *Virter) vmShutdown(afterNotifier AfterNotifier, shutdownTimeout time.Duration, domain libvirt.Domain) error {
	events, err := v.libvirt.LifecycleEvents()
	if err != nil {
		return fmt.Errorf("could not start waiting for events: %w", err)
	}

	// Check whether domain is active after starting event stream
	// to ensure that the shutdown event is not missed.
	active, err := v.libvirt.DomainIsActive(domain)
	if err != nil {
		return fmt.Errorf("could not check if domain is active: %w", err)
	}

	if active != 0 {
		log.Printf("Shut down VM")

		err = v.libvirt.DomainShutdown(domain)
		if err != nil {
			return fmt.Errorf("could not shut down domain: %w", err)
		}

		log.Printf("Wait for VM to stop")
	}

	timeout := afterNotifier.After(shutdownTimeout)

	for active != 0 {
		select {
		case event := <-events:
			if event.Dom.ID == domain.ID && event.Event == int32(libvirt.DomainEventStopped) {
				log.Printf("VM stopped")
				active = 0
			}
		case <-timeout:
			return fmt.Errorf("timed out waiting for domain to stop")
		}
	}

	return nil
}

func (v *Virter) getIP(vmName string, network *libvirt.Network) (string, error) {
	domain, err := v.libvirt.DomainLookupByName(vmName)
	if err != nil {
		return "", fmt.Errorf("could not get domain '%s': %w", vmName, err)
	}

	active, err := v.libvirt.DomainIsActive(domain)
	if err != nil {
		return "", fmt.Errorf("could not check if domain '%s' is active: %w", vmName, err)
	}

	if active == 0 {
		return "", fmt.Errorf("cannot exec against VM '%s' that is not running", vmName)
	}

	if network == nil {
		lookup, err := v.libvirt.NetworkLookupByName(v.networkName)
		if err != nil {
			return "", fmt.Errorf("could not get network: %w", err)
		}

		network = &lookup
	}

	ip, err := v.findVMIP(*network, domain)
	if err != nil {
		return "", fmt.Errorf("could not find IP for VM '%s': %w", vmName, err)
	}

	return ip, nil
}

func (v *Virter) getIPs(vmNames []string) ([]string, error) {
	var ips []string
	network, err := v.libvirt.NetworkLookupByName(v.networkName)
	if err != nil {
		return ips, fmt.Errorf("could not get network: %w", err)
	}

	for _, vmName := range vmNames {
		ip, err := v.getIP(vmName, &network)
		if err != nil {
			return nil, err
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

// VMExecDocker runs a docker container against some VMs.
func (v *Virter) VMExecDocker(ctx context.Context, docker DockerClient, vmNames []string, dockerContainerConfig DockerContainerConfig, sshPrivateKey []byte) error {
	ips, err := v.getIPs(vmNames)
	if err != nil {
		return err
	}

	return dockerRun(ctx, docker, dockerContainerConfig, ips, sshPrivateKey)
}

func getSSHClientConfig(sshPrivateKey []byte) (ssh.ClientConfig, error) {
	signer, err := ssh.ParsePrivateKey(sshPrivateKey)
	if err != nil {
		return ssh.ClientConfig{}, err
	}

	config := ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	return config, nil
}

// VMSSHSession runs an interactive shell session in a VM
func (v *Virter) VMSSHSession(ctx context.Context, vmName string, sshPrivateKey []byte) error {
	ips, err := v.getIPs([]string{vmName})
	if err != nil {
		return err
	}
	if len(ips) != 1 {
		return fmt.Errorf("Expected a single IP")
	}

	sshConfig, err := getSSHClientConfig(sshPrivateKey)
	if err != nil {
		return err
	}

	hostPort := net.JoinHostPort(ips[0], "22")
	sshClient := sshclient.NewSSHClient(hostPort, sshConfig)
	if err := sshClient.Dial(); err != nil {
		return err
	}
	defer sshClient.Close()

	return sshClient.Shell()
}

// VMExecShell runs a simple shell command against some VMs.
func (v *Virter) VMExecShell(ctx context.Context, vmNames []string, sshPrivateKey []byte, shellStep *ProvisionShellStep) error {
	ips, err := v.getIPs(vmNames)
	if err != nil {
		return err
	}

	sshConfig, err := getSSHClientConfig(sshPrivateKey)
	if err != nil {
		return err
	}

	var g errgroup.Group
	for i, ip := range ips {
		ip := ip
		vmName := vmNames[i]
		log.Println("Provisioning via SSH:", shellStep.Script, "in", ip)
		g.Go(func() error {
			return runSSHCommand(ctx, &sshConfig, vmName, net.JoinHostPort(ip, "22"), shellStep.Script, EnvmapToSlice(shellStep.Env))
		})
	}

	return g.Wait()
}

func (v *Virter) VMExecRsync(ctx context.Context, copier netcopy.NetworkCopier, vmNames []string, rsyncStep *ProvisionRsyncStep) error {
	files, err := filepath.Glob(rsyncStep.Source)
	if err != nil {
		return fmt.Errorf("failed to parse glob pattern: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, vmName := range vmNames {
		vmName := vmName
		log.Printf(`Copying files via rsync: %s to %s on %s`, rsyncStep.Source, rsyncStep.Dest, vmNames)
		g.Go(func() error {
			dest := fmt.Sprintf("%s:%s", vmName, rsyncStep.Dest)
			return v.VMExecCopy(ctx, copier, files, dest)
		})
	}
	return g.Wait()
}

func (v *Virter) VMExecCopy(ctx context.Context, copier netcopy.NetworkCopier, sourceSpecs []string, destSpec string) error {
	sources := make([]netcopy.HostPath, len(sourceSpecs))
	for i, srcSpec := range sourceSpecs {
		sources[i] = netcopy.ParseHostPath(srcSpec)

		if !sources[i].Local() {
			// Replace hostname with ip
			ip, err := v.getIP(sources[i].Host, nil)
			if err != nil {
				return err
			}
			sources[i].Host = ip
		}
	}

	dest := netcopy.ParseHostPath(destSpec)
	if !dest.Local() {
		ip, err := v.getIP(dest.Host, nil)
		if err != nil {
			return err
		}
		dest.Host = ip
	}

	return copier.Copy(ctx, sources, dest)
}

func runSSHCommand(ctx context.Context, config *ssh.ClientConfig, vmName, ipPort, script string, env []string) error {
	script, err := sshclient.AddEnv(script, env)
	if err != nil {
		return err
	}

	// Retry connection until the context is cancelled. We expect to have
	// already formed a successful SSH connection before we do any
	// provisioning over SSH. This is a workaround for VMs that make SSH
	// available but then temporarily stop it again.
	sshClient, err := connectSSHRetry(ctx, config, ipPort)
	if err != nil {
		return err
	}
	defer sshClient.Close()

	outp, err := sshClient.StdoutPipe()
	if err != nil {
		return err
	}
	errp, err := sshClient.StderrPipe()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go logLines(&wg, vmName, false, outp)
	go logLines(&wg, vmName, true, errp)

	err = sshClient.ExecScript(script)
	wg.Wait()

	return err
}

func connectSSHRetry(ctx context.Context, config *ssh.ClientConfig, ipPort string) (*sshclient.SSHClient, error) {
	var sshClient *sshclient.SSHClient
	for sshClient == nil {
		sshClient = sshclient.NewSSHClient(ipPort, *config)
		if err := sshClient.DialContext(ctx); err != nil {
			select {
			case <-ctx.Done():
				return nil, err
			case <-time.After(time.Second):
			}
			log.Warnf("Retrying SSH connection due to failure: %v", err)
			sshClient = nil
		}
	}
	return sshClient, nil
}

func (v *Virter) findVMIP(network libvirt.Network, domain libvirt.Domain) (string, error) {
	mac, err := v.getMAC(domain)
	if err != nil {
		return "", err
	}

	ips, err := v.findIPs(network, mac)
	if err != nil {
		return "", err
	}
	if len(ips) < 1 {
		return "", fmt.Errorf("no IP found for domain")
	}

	return ips[0], nil
}
